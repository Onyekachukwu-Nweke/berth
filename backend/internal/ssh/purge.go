package ssh

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/goodylili/mountabo/internal/usecase"
)

var _ usecase.AppPurger = (*Client)(nil)

// purgeScript removes everything one deployment owns on a server, selecting by
// OWNERSHIP and validating every path before it deletes anything.
//
// Selecting by name cannot work: names were derived from the app and branch,
// values two deployments can share, so a name-based sweep could remove a
// bystander's containers. Every resource mountabo creates carries
// mountabo.deployment=<id>, and compose additionally sets
// com.docker.compose.project to the same id.
//
// It is resumable. Each step appends its outcome to a journal, so a purge
// interrupted halfway can be retried and the operator can be told what is still
// there rather than being told "done" when it is not.
//
// The state document and lock file are deliberately NOT removed: the tombstone is
// what stops a run queued before the deletion from recreating the app.
func purgeScript(spec usecase.PurgeSpec) string {
	id := shellSingleQuote(spec.DeploymentID)
	repo := "mountabo/" + spec.DeploymentID

	volumes := `echo "kept: named volumes (databases and uploads live here)" >> "$JOURNAL"`
	if spec.RemoveVolumes {
		volumes = `vols=$(docker volume ls -q --filter "label=com.docker.compose.project=` + spec.DeploymentID + `" 2>/dev/null || true)
if [ -n "$vols" ]; then
  echo "$vols" | xargs -r docker volume rm 2>/dev/null && echo "removed: volumes" >> "$JOURNAL" || echo "INCOMPLETE: volumes" >> "$JOURNAL"
else
  echo "removed: volumes (none)" >> "$JOURNAL"
fi`
	}

	deployKey := `echo "kept: deploy key (another environment of this repo still uses it)" >> "$JOURNAL"`
	if spec.RemoveDeployKey && strings.TrimSpace(spec.DeployKeyFile) != "" {
		deployKey = `key="$HOME/.ssh/"` + shellSingleQuote(spec.DeployKeyFile) + `
rm -f "$key" && echo "removed: deploy key" >> "$JOURNAL" || echo "INCOMPLETE: deploy key" >> "$JOURNAL"`
	}

	return fmt.Sprintf(`set -uo pipefail
DEPLOYMENT_ID=%[1]s
STATE_DIR="$HOME/.mountabo/state"
JOURNAL="$HOME/.mountabo/purge-$DEPLOYMENT_ID.log"
mkdir -p "$STATE_DIR" "$HOME/.mountabo/locks"
umask 077
: > "$JOURNAL"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is not installed on this server" >> "$JOURNAL"
fi

# Containers, by ownership label. Both filters name the same deployment.
if command -v docker >/dev/null 2>&1; then
  ids=$(docker ps -aq --filter "label=com.docker.compose.project=$DEPLOYMENT_ID" --filter "label=mountabo.deployment=$DEPLOYMENT_ID" 2>/dev/null | sort -u)
  if [ -n "$ids" ]; then
    echo "$ids" | xargs -r docker rm -f >/dev/null 2>&1 && echo "removed: containers" >> "$JOURNAL" || echo "INCOMPLETE: containers" >> "$JOURNAL"
  else
    echo "removed: containers (none)" >> "$JOURNAL"
  fi

  # Networks this deployment created. A network it merely joined carries someone
  # else's project label and is left alone.
  nets=$(docker network ls -q --filter "label=com.docker.compose.project=$DEPLOYMENT_ID" 2>/dev/null || true)
  [ -z "$nets" ] || echo "$nets" | xargs -r docker network rm >/dev/null 2>&1 || true
  echo "removed: networks" >> "$JOURNAL"

  %[5]s

  # Images from this deployment's own repository only. An image can carry other
  # tags or back another deployment's containers, so this removes TAGS and lets
  # docker refuse anything still in use — that refusal is correct, not an error.
  imgs=$(docker images --filter=reference=%[2]s --filter=reference=%[3]s --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | sort -u)
  [ -z "$imgs" ] || echo "$imgs" | xargs -r -n1 docker rmi >/dev/null 2>&1 || true
  echo "removed: images" >> "$JOURNAL"
fi

# Filesystem removal, validated rather than string-prefixed. realpath resolves
# symlinks and "..", so a crafted or mistaken path cannot escape the deploy root;
# the containment check then confirms the target really is inside it.
purge_dir() {
  __what="$1"; __root="$2"; __target="$3"
  if [ -z "$__target" ] || [ "$__target" = "/" ]; then
    echo "INCOMPLETE: $__what (refused: empty or root path)" >> "$JOURNAL"; return
  fi
  if [ ! -e "$__target" ]; then
    echo "removed: $__what (absent)" >> "$JOURNAL"; return
  fi
  __rp="$(realpath "$__target" 2>/dev/null || true)"
  __rr="$(realpath "$__root" 2>/dev/null || true)"
  if [ -z "$__rp" ] || [ -z "$__rr" ] || [ "$__rp" = "/" ]; then
    echo "INCOMPLETE: $__what (refused: could not resolve $__target)" >> "$JOURNAL"; return
  fi
  case "$__rp/" in
    "$__rr"/*) : ;;
    *) echo "INCOMPLETE: $__what (refused: $__rp is outside $__rr)" >> "$JOURNAL"; return ;;
  esac
  if [ ! -O "$__rp" ]; then
    echo "INCOMPLETE: $__what (refused: $__rp is not owned by this user)" >> "$JOURNAL"; return
  fi
  rm -rf "$__rp" && echo "removed: $__what" >> "$JOURNAL" || echo "INCOMPLETE: $__what" >> "$JOURNAL"
}

purge_dir "deploy directory" %[4]s %[4]s/$DEPLOYMENT_ID
purge_dir "staging directory" /tmp /tmp/$DEPLOYMENT_ID-deploy
purge_dir "release bundles" "$HOME/.mountabo/releases" "$HOME/.mountabo/releases/$DEPLOYMENT_ID"

%[6]s

# The state document is the tombstone: a run queued before this deletion must
# still find "deleted" and stop rather than recreating what was just removed. It
# and the lock file therefore survive the purge, permanently.
tmp="$STATE_DIR/$DEPLOYMENT_ID.json.tmp.$$"
if [ -f "$STATE_DIR/$DEPLOYMENT_ID.json" ]; then
  jq -c '.lifecycle = "deleted" | .active = null | .previous = null | .candidate = null | .running = null' \
    "$STATE_DIR/$DEPLOYMENT_ID.json" > "$tmp" 2>/dev/null && mv -f "$tmp" "$STATE_DIR/$DEPLOYMENT_ID.json"
else
  jq -n --arg d "$DEPLOYMENT_ID" '{deployment_id: $d, lifecycle: "deleted", incarnation: 1, generation: 1}' > "$tmp" \
    && mv -f "$tmp" "$STATE_DIR/$DEPLOYMENT_ID.json"
fi
rm -f "$tmp"

echo "--- purge summary ---"
cat "$JOURNAL"
if grep -q '^INCOMPLETE' "$JOURNAL"; then
  echo "cleanup incomplete: the entries above marked INCOMPLETE are still on this server"
  exit 3
fi
echo "cleanup complete"
`, id, shellSingleQuote(repo+":*"), shellSingleQuote(repo+"-*:*"),
		shellSingleQuote(strings.TrimRight(spec.DeployDir, "/")), volumes, deployKey)
}

// PurgeApp removes the resources one deployment owns, streaming its journal.
//
// Run as root: the deploy directory can live under a root-owned path such as
// /opt, which the deploy user cannot remove. Every path is validated for
// containment and ownership before deletion precisely because this runs as root.
func (c *Client) PurgeApp(ctx context.Context, t usecase.SSHTarget, spec usecase.PurgeSpec, out io.Writer) error {
	if err := c.RunAsRoot(ctx, t, purgeScript(spec), out); err != nil {
		return fmt.Errorf("purge %s: %w", spec.DeploymentID, err)
	}
	return nil
}
