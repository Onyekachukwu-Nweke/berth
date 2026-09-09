// Package workflow generates the exact files mountabo commits to a repo to
// deploy it over SSH: a GitHub Actions workflow that copies deploy.sh to the
// server and runs it, plus deploy.sh itself. Generation is pure (config in,
// text out) and is the single source of truth, the configure UI previews these
// same bytes by calling the backend, so the preview always matches what gets
// committed.
//
// Two deploy strategies are supported. "compose" drives a docker compose stack
// (the repo has a Compose file); "docker" builds the repo's Dockerfile and runs
// a single container, publishing each EXPOSE'd port to an editable host port.
package workflow

import (
	"fmt"
	"strings"
)

// Strategy selects how the committed deploy.sh ships the app.
type Strategy string

const (
	// Compose deploys with `docker compose` (the repo has a Compose file).
	Compose Strategy = "compose"
	// Docker builds the repo's Dockerfile and runs one container.
	Docker Strategy = "docker"
)

// Port is one published port. EnvVar/Value drive a compose stack's host port
// (written into .env and referenced by the Compose file); Value/Container drive
// a plain `docker run -p Value:Container` mapping. Value is the host port the
// operator chose; Container is the port the app listens on inside.
type Port struct {
	EnvVar    string
	Value     string
	Container string
}

// EnvVar is one application environment variable; its value is a GitHub Actions
// secret, the generated files only ever reference it by name.
type EnvVar struct {
	Key   string
	Value string
}

// Config is everything the generated files derive from. Environment is the
// GitHub deployment environment whose secrets the workflow uses; it defaults to
// Branch. Strategy defaults to Compose when empty.
type Config struct {
	App         string
	Owner       string
	Repo        string
	Branch      string
	Environment string
	RootDir     string
	DeployDir   string
	Strategy    Strategy
	Ports       []Port
	EnvVars     []EnvVar
	// DeployKeyFile, when set, is the name of the repo's read-only deploy key
	// under the deploy user's ~/.ssh; deploy.sh uses it for git over SSH. Empty
	// falls back to whatever key the server's git already has.
	DeployKeyFile string
	// DeploymentID is the canonical identity every server-side resource is named
	// from (usecase.DeriveDeploymentID): the committed script, the staging
	// directory, the checkout, the compose project, container names, the image
	// namespace, the lock and the state document. Empty only in a preview
	// rendered before the id is known, which falls back to the environment.
	DeploymentID string
	// Incarnation is how many times this deployment has been created. Baked into
	// the generated script so a run queued against a deleted deployment, whose
	// successor has since been recreated, is rejected rather than deploying into
	// it. Zero is treated as the first incarnation.
	Incarnation int
}

// scriptDir holds the per-deployment scripts, one file per deployment.
const scriptDir = ".mountabo/"

// ScriptPath is where this deployment's script is committed. It is keyed on the
// canonical deployment id rather than the branch or app name: a single repo-root
// deploy.sh meant two environments of one repo on the same branch overwrote each
// other's strategy, ports and deploy directory.
func ScriptPath(c Config) string { return scriptDir + ScriptName(c) }

// ScriptName is the script's bare file name, which is also what it is called on
// the server after the transfer flattens it by basename.
func ScriptName(c Config) string { return "deploy-" + c.deploymentID() + ".sh" }

// StagingDir is where the workflow ships the script and image tarball on the
// server. Scoped to the deployment so two environments never overwrite each
// other's artifacts mid-transfer.
func StagingDir(c Config) string { return "/tmp/" + c.deploymentID() + "-deploy" }

// deploymentID falls back to the environment when no id was supplied, so a
// preview rendered before the id is known still produces coherent paths.
func (c Config) deploymentID() string {
	if id := strings.TrimSpace(c.DeploymentID); id != "" {
		return id
	}
	return c.environment()
}

// incarnation defaults to the first, so a preview or a config built without it
// renders a script that is not treated as superseded.
func (c Config) incarnation() int {
	if c.Incarnation < 1 {
		return 1
	}
	return c.Incarnation
}

func (c Config) environment() string {
	if e := strings.TrimSpace(c.Environment); e != "" {
		return e
	}
	return c.Branch
}

func (c Config) strategy() Strategy {
	if c.Strategy == Docker {
		return Docker
	}
	return Compose
}

// envNames returns the application env var keys, in order, dropping blanks.
func (c Config) envNames() []string {
	out := make([]string, 0, len(c.EnvVars))
	for _, v := range c.EnvVars {
		if k := strings.TrimSpace(v.Key); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// FileName is the generated workflow's file name, one per environment. This is
// the single source of the naming convention; callers that need the bare name
// (the deploy record, the monitor, the run walkthrough) use it instead of
// re-deriving the string.
func FileName(environment string) string {
	return fmt.Sprintf("mountabo-deploy-%s.yml", environment)
}

// Path is the in-repo path of the generated workflow, one per environment.
func Path(c Config) string {
	return ".github/workflows/" + FileName(c.environment())
}

// imageRepo is the deterministic image repository mountabo tags a build under,
// namespaced per DEPLOYMENT rather than per app. Namespacing it by app meant two
// environments of one app produced the identical tag for the same commit, so
// staging's image and production's image were the same object on the server and
// on the runner. The runner tags `<imageRepo>:<sha>` (docker) or
// `<imageRepo>-<service>:<sha>` (compose) and the server loads exactly that.
// Docker repository names must be lowercase; the deployment id already is.
func (c Config) imageRepo() string {
	return "mountabo/" + c.deploymentID()
}

// Workflow renders the GitHub Actions workflow YAML. The build happens on the
// runner (not the server): the runner builds the image(s), `docker save | zstd`s
// them to a tarball, and rsyncs the tarball + deploy.sh (+ a compose image
// override) to the box, which only loads and runs. The build/transfer steps are
// strategy-aware; the run step is shared. The passthrough env block and envs
// list cover the operator's env vars plus DEPLOY_DIR (all from secrets) and, for
// the docker strategy, MOUNTABO_IMAGE (the exact tag to run); the job pins the
// deployment environment so its secrets resolve. MOUNTABO_ENV_KEYS (the
// operator's env-var names) lets deploy.sh assemble .env from the forwarded
// secrets without the names being baked into the script — so a secret added
// later only needs this workflow regenerated, not the script.
func Workflow(c Config) string {
	appKeys := c.envNames()
	// Secrets forwarded per-name into the ssh step: the operator's vars + the
	// deploy directory. MOUNTABO_ENV_KEYS carries just the operator var names.
	names := append(append([]string{}, appKeys...), "DEPLOY_DIR")

	var envBlock strings.Builder
	for _, n := range names {
		fmt.Fprintf(&envBlock, "          %s: ${{ secrets.%s }}\n", n, n)
	}
	fmt.Fprintf(&envBlock, "          MOUNTABO_ENV_KEYS: %q", strings.Join(appKeys, " "))

	forwardNames := append(append([]string{}, names...), "MOUNTABO_ENV_KEYS")
	// The docker strategy runs one known image tag; forward it so deploy.sh runs
	// exactly what the runner built and loaded. Compose gets the mapping from the
	// image-override file instead, so it needs no extra env.
	if c.strategy() == Docker {
		fmt.Fprintf(&envBlock, "\n          MOUNTABO_IMAGE: %s:${{ github.sha }}", c.imageRepo())
		forwardNames = append(forwardNames, "MOUNTABO_IMAGE")
	}
	forward := strings.Join(forwardNames, ",")

	return fmt.Sprintf(`name: %[1]s deploy (%[2]s)

on:
  push:
    branches:
      - %[2]s
  workflow_dispatch:

# Serialize deploys to this target: while a run is in progress, a newer run
# queues behind it rather than cancelling it, so a half-applied deploy is never
# interrupted. Group is per app+environment to match mountabo's local deploy lock.
concurrency:
  group: mountabo-deploy-%[1]s-%[3]s
  cancel-in-progress: false

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: %[3]s
    steps:
      - name: Checkout code
        uses: actions/checkout@v4

%[6]s
      - name: Ship image and deploy script to the server
        timeout-minutes: 10
        env:
          SERVER_HOST: ${{ secrets.SERVER_HOST }}
          SERVER_USER: ${{ secrets.SERVER_USER }}
          SERVER_SSH_KEY: ${{ secrets.SERVER_SSH_KEY }}
        run: |
          # rsync the built image tarball + deploy.sh (+ compose image override)
          # to the box over SSH. rsync --partial resumes a dropped transfer; no -z
          # because the tarball is already zstd-compressed. The key never lands in
          # the repo — it is a run-scoped temp file removed on exit.
          key="$(mktemp)"
          trap 'rm -f "$key"' EXIT
          printf '%%s\n' "$SERVER_SSH_KEY" > "$key"
          chmod 600 "$key"
          ssh_opts="ssh -i $key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
          ssh -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            "$SERVER_USER@$SERVER_HOST" "mkdir -p %[8]s"
          # The deploy script is always shipped; the tarball (and compose image
          # override) only when the build produced them — a pull-only compose
          # stack has none.
          srcs="%[9]s"
          for f in %[7]s; do [ -f "$f" ] && srcs="$srcs $f"; done
          rsync -a --partial -e "$ssh_opts" $srcs \
            "$SERVER_USER@$SERVER_HOST:%[8]s/"

      - name: Deploy on the server
        uses: appleboy/ssh-action@v1.1.0
        env:
%[4]s
        with:
          host: ${{ secrets.SERVER_HOST }}
          username: ${{ secrets.SERVER_USER }}
          key: ${{ secrets.SERVER_SSH_KEY }}
          envs: %[5]s
          script: |
            chmod +x %[8]s/%[10]s
            %[8]s/%[10]s %[2]s
`, c.App, c.Branch, c.environment(), envBlock.String(), forward, buildStep(c), rsyncArtifacts(c),
		StagingDir(c), ScriptPath(c), ScriptName(c))
}

// buildStep renders the runner build+save step for the strategy: it builds the
// image(s) and writes image.tar.zst next to deploy.sh so the transfer step can
// ship them. The image tag is derived from github.sha so each commit loads a
// distinct, known tag on the server.
func buildStep(c Config) string {
	root := rootDirOrDot(c)
	if c.strategy() == Docker {
		// Single image from the repo's Dockerfile; context is the app root.
		return fmt.Sprintf(`      - name: Build image on the runner
        run: |
          docker build -t %[1]s:${{ github.sha }} %[2]s
          docker save %[1]s:${{ github.sha }} | zstd -T0 -o image.tar.zst
`, c.imageRepo(), root)
	}
	// Compose: pin every build-service to a deterministic tag via a generated
	// image override, build with it, then save only those tags. jq enumerates the
	// services that declare a build: (pulled images are left for the server to
	// pull). This all runs on the runner, so the box needs no jq and no modern
	// compose. The same override is rsynced to the server (rsyncSources) so
	// `up --no-build` resolves the loaded tags there.
	return fmt.Sprintf(`      - name: Build compose images on the runner
        run: |
          cd %[2]s
          compose=""
          for cf in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
            if [ -f "$cf" ]; then compose="$cf"; break; fi
          done
          [ -n "$compose" ] || { echo "no compose file found under %[2]s" >&2; exit 1; }
          # --no-interpolate: only the structure is needed (which services declare a
          # build:), and the runner has no .env, so unset ${VARS} must not fail here.
          services=$(docker compose -f "$compose" config --no-interpolate --format json \
            | jq -r '.services | to_entries[] | select(.value.build != null) | .key')
          tags=""
          rm -f mountabo.image.override.yml
          for s in $services; do
            svc=$(printf '%%s' "$s" | tr '[:upper:]' '[:lower:]')
            tag="%[1]s-$svc:${{ github.sha }}"
            [ -f mountabo.image.override.yml ] || echo "services:" > mountabo.image.override.yml
            printf '  %%s:\n    image: %%s\n' "$s" "$tag" >> mountabo.image.override.yml
            tags="$tags $tag"
          done
          # A compose stack with no build: services has nothing to offload — its
          # images are all pulled, so the server just pulls them on up. Skip the
          # tarball + override entirely (the ship + load steps handle their absence).
          if [ -n "$tags" ]; then
            docker compose -f "$compose" -f mountabo.image.override.yml build
            docker save $tags | zstd -T0 -o image.tar.zst
          else
            echo "no build: services in compose; images will be pulled on the server"
          fi
`, c.imageRepo(), root)
}

// ownershipLabels are the labels stamped on every resource this deployment
// creates, as a space-separated "k=v" list. Teardown selects on
// mountabo.deployment to remove exactly this deployment's containers, networks
// and volumes — rather than guessing from names, which cannot distinguish two
// deployments that happen to share an app or a branch.
func (c Config) ownershipLabels() string {
	return strings.Join([]string{
		"mountabo.deployment=" + c.deploymentID(),
		"mountabo.owner=" + strings.ToLower(strings.TrimSpace(c.Owner)),
		"mountabo.repo=" + strings.ToLower(strings.TrimSpace(c.Repo)),
		"mountabo.environment=" + c.environment(),
	}, " ")
}

// ownershipLabelArgs is the same set, quoted for use as individual --label
// arguments in a docker run invocation.
func (c Config) ownershipLabelArgs() []string {
	parts := strings.Split(c.ownershipLabels(), " ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, fmt.Sprintf("%q", p))
	}
	return out
}

// rsyncArtifacts is the space-separated list of build outputs the transfer step
// ships alongside deploy.sh (which it always ships). The ship step guards each
// with a `[ -f ]` test, so a pull-only compose stack that produced no tarball or
// override simply ships neither. Docker writes image.tar.zst at the workspace
// root; the compose build step cd's into the app root first, so its artifacts
// carry that prefix. rsync flattens them into the deployment's staging
// directory by basename.
func rsyncArtifacts(c Config) string {
	if c.strategy() == Docker {
		return "image.tar.zst"
	}
	root := rootDirOrDot(c)
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}
	return fmt.Sprintf("%[1]simage.tar.zst %[1]smountabo.image.override.yml", prefix)
}

// rootDirOrDot is the app root relative to the repo, or "." at the repo root.
func rootDirOrDot(c Config) string {
	root := strings.TrimRight(strings.TrimLeft(c.RootDir, "./"), "/")
	if root == "" {
		return "."
	}
	return root
}

// DeployScript renders deploy.sh for the configured strategy.
func DeployScript(c Config) string {
	if c.strategy() == Docker {
		return dockerScript(c)
	}
	return composeScript(c)
}

// header is the shared top of every deploy.sh: strict mode, the branch and
// deploy directory, the repo's SSH URL, and a banner.
func header(c Config) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

BRANCH="${1:-%[1]s}"
DEPLOYMENT_ID=%[6]q
DEPLOY_DIR="${DEPLOY_DIR:-%[2]s}/$DEPLOYMENT_ID"
REPO_URL="git@github.com:%[3]s/%[4]s.git"

echo "=== %[5]s deploy (branch: $BRANCH) ==="`, c.Branch, c.DeployDir, c.Owner, c.Repo, c.App, c.deploymentID())
}

// lockPreamble serializes the whole operation on the server and refuses to run
// against a deleted or superseded deployment. It is emitted into every generated
// script, immediately after the header.
//
// The lock has to live on the box. mountabo's in-process DeployLocker only
// covers operations mountabo itself starts, and a GitHub Actions concurrency
// group only covers Actions runs — neither can see an SSH-direct rollback, hook
// or teardown. A file lock on the server is the one point all of them pass
// through, so it is the only real serialization boundary.
//
// It fails closed. flock is installed and verified during bootstrap; if it is
// missing here the guarantee is simply absent, and continuing would risk two
// operations interleaving on one working tree — so the script stops instead.
//
// The state document doubles as the deletion tombstone. It outlives a teardown
// on purpose: a run queued before a deployment was deleted must find "deleted"
// and stop, rather than recreating the app that was just torn down. Recreating a
// deployment bumps its incarnation, so a run generated under an older one is
// rejected as superseded instead of deploying into its successor.
func lockPreamble(c Config) string {
	return fmt.Sprintf(`
# --- mountabo: serialize on the server, and refuse superseded work ------------
MOUNTABO_STATE="${MOUNTABO_STATE:-$HOME/.mountabo}"
MOUNTABO_MODE="${MOUNTABO_MODE:-deploy}"
STATE_FILE="$MOUNTABO_STATE/state/$DEPLOYMENT_ID.json"
LOCK_FILE="$MOUNTABO_STATE/locks/$DEPLOYMENT_ID.lock"
EXPECT_INCARNATION=%[1]d

umask 077
mkdir -p "$MOUNTABO_STATE/state" "$MOUNTABO_STATE/locks" "$MOUNTABO_STATE/releases"
chmod 700 "$MOUNTABO_STATE" 2>/dev/null || true

if ! command -v flock >/dev/null 2>&1; then
  echo "error: flock is missing on this server, so mountabo cannot serialize this deploy" >&2
  echo "       install it (apt-get install -y util-linux) and re-run setup for this server" >&2
  exit 1
fi
exec 9>"$LOCK_FILE"
if ! flock -w "${MOUNTABO_LOCK_WAIT:-900}" 9; then
  echo "error: another operation on this deployment holds the lock; it did not finish in time" >&2
  exit 1
fi

# Everything below runs under the lock, so the state read cannot race a teardown.
if [ -f "$STATE_FILE" ]; then
  state_lifecycle="$(jq -r '.lifecycle // "active"' "$STATE_FILE" 2>/dev/null || echo active)"
  state_incarnation="$(jq -r '.incarnation // 1' "$STATE_FILE" 2>/dev/null || echo 1)"
  case "$state_lifecycle" in
    deleting|deleted)
      echo "This deployment was deleted in mountabo, so this run will not deploy."
      echo "Recreate it from mountabo if you want it back; that resets its state deliberately."
      exit 0
      ;;
    blocked)
      echo "error: this deployment is blocked on an unresolved migration outcome." >&2
      echo "       Resolve it in mountabo (confirm whether the migration applied) before deploying." >&2
      exit 1
      ;;
  esac
  if [ "$state_incarnation" -gt "$EXPECT_INCARNATION" ] 2>/dev/null; then
    echo "This run was generated for an earlier incarnation of this deployment (#$EXPECT_INCARNATION,"
    echo "now #$state_incarnation), so it has been superseded and will not deploy."
    exit 0
  fi
fi
`, c.incarnation())
}

// gitAuth points git at the repo's read-only deploy key for SSH clones, when
// one is configured. mountabo installs that key on the server out of band.
func gitAuth(c Config) string {
	if c.DeployKeyFile == "" {
		return ""
	}
	return fmt.Sprintf(`# Use the repo's read-only deploy key for git over SSH.
export GIT_SSH_COMMAND="ssh -i $HOME/.ssh/%s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"

`, c.DeployKeyFile)
}

// clonePull clones the repo on first deploy, or fast-forwards it on later ones.
// DEPLOY_DIR often lives under a root-owned path such as /opt, where the deploy
// user cannot create directories, so it is created with sudo (the bootstrap
// grants the deploy user passwordless sudo) and then handed to the deploy user
// so git and docker run without root from here on.
const clonePull = `sudo mkdir -p "$DEPLOY_DIR"
sudo chown -R "$(id -un):$(id -gn)" "$(dirname "$DEPLOY_DIR")"

if [ -d "$DEPLOY_DIR/.git" ]; then
  echo "Pulling latest changes..."
  cd "$DEPLOY_DIR"
  git fetch origin
  git reset --hard "origin/$BRANCH"
else
  echo "Cloning repository..."
  rm -rf "$DEPLOY_DIR"
  git clone --branch "$BRANCH" "$REPO_URL" "$DEPLOY_DIR"
  cd "$DEPLOY_DIR"
fi`

// populateEnvFiles copies the .env just written into any per-service env file
// the compose declares (env_file:), since those files are not committed to the
// repo. It only touches paths ending in .env and only when the file is missing,
// so a committed env file is never overwritten and unrelated list items (ports,
// volumes) are ignored. Paths are relative to the compose file in the current
// directory, matching how compose resolves env_file.
const populateEnvFiles = `# Create any per-service env files the compose references (env_file:) from the
# .env above, so the configured variables reach every service.
for cf in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
  [ -f "$cf" ] || continue
  awk '
    function emit(p){ gsub(/^[ \t]+|[ \t]+$/,"",p); gsub(/^"|"$/,"",p); if(p!="") print p }
    /^[ \t]*env_file:[ \t]*[^ \t#]/ { line=$0; sub(/^[ \t]*env_file:[ \t]*/,"",line); emit(line); blk=0; next }
    /^[ \t]*env_file:[ \t]*$/ { blk=1; next }
    blk { if ($0 ~ /^[ \t]*-[ \t]*/) { line=$0; sub(/^[ \t]*-[ \t]*/,"",line); emit(line) } else if ($0 ~ /[^ \t]/) { blk=0 } }
  ' "$cf"
done | sort -u | while IFS= read -r ef; do
  case "$ef" in
    *.env) mkdir -p "$(dirname "$ef")"; [ -f "$ef" ] || cp .env "$ef" ;;
  esac
done

`

// composeEnvOverride mounts the operator's .env into every compose service as
// container environment. Docker Compose reads a project .env only for ${VAR}
// interpolation in the compose file, NOT as container env — so a service that
// declares no env_file:/environment: never receives the operator's secrets. This
// generates a mountabo-owned override that attaches `env_file: .env` to each
// service and merges it in with -f. Existing env_file lists are preserved, and
// if both an existing env_file and .env define the same key, the operator's .env
// is later in the chain; explicit service `environment:` still wins over env_file.
// It reads only service NAMES (docker compose config --services); the user's
// compose is never parsed or edited. COMPOSE_ARGS is the -f list (base [+ the
// user's own override] + this override) used for build/up/ps; it stays empty when
// no compose file is found so compose falls back to auto-detection. The override
// file is regenerated each deploy and never committed (git reset --hard leaves
// untracked files, so it simply persists/refreshes).
func composeEnvOverride(c Config) string {
	return fmt.Sprintf(`# Mount the operator's .env into every compose service as container environment
# (compose otherwise uses .env only for interpolation, not as container env).
# Stamped onto every service so teardown can select this deployment's containers
# by ownership rather than by guessing at names. Compose also labels them with
# com.docker.compose.project, which is the deployment id.
MOUNTABO_LABELS=%[1]q
COMPOSE=""
for cf in docker-compose.yml docker-compose.yaml compose.yml compose.yaml; do
  if [ -f "$cf" ]; then COMPOSE="$cf"; break; fi
done
COMPOSE_ARGS=""
if [ -n "$COMPOSE" ]; then
  COMPOSE_ARGS="-f $COMPOSE"
  for ov in docker-compose.override.yml docker-compose.override.yaml compose.override.yml compose.override.yaml; do
    if [ -f "$ov" ]; then COMPOSE_ARGS="$COMPOSE_ARGS -f $ov"; break; fi
  done
  services=$(docker compose $COMPOSE_ARGS config --services 2>/dev/null || true)
  if [ -n "$services" ]; then
    {
      echo "services:"
      for s in $services; do
        printf '  %%s:\n    env_file:\n      - .env\n    labels:\n' "$s"
        for l in $MOUNTABO_LABELS; do printf '      - %%s\n' "$l"; done
      done
    } > mountabo.env.override.yml
    COMPOSE_ARGS="$COMPOSE_ARGS -f mountabo.env.override.yml"
    echo "mounting .env into services: $services"
  fi
fi

`, c.ownershipLabels())
}

// cdRoot is the `cd <rootDir>` line (with trailing newline) when the app lives
// in a sub-directory, or "" for the repo root.
func cdRoot(c Config) string {
	root := strings.TrimRight(strings.TrimLeft(c.RootDir, "./"), "/")
	if root == "" {
		return ""
	}
	return fmt.Sprintf("cd %q\n", root)
}

// envWriteBlock is the shell that writes .env at deploy time. It seeds the file
// with the baked port vars (compose only — the Compose file reads them; docker
// publishes ports with -p flags instead), then appends one line per operator
// variable. The variable NAMES come from $MOUNTABO_ENV_KEYS (set by the
// workflow from the configured secret names) and the VALUES from the matching
// forwarded secrets — so the names are not baked into deploy.sh and a secret
// added later reaches the container once the workflow is regenerated. It also
// reports how many variables were written, warns on empties, and fails the run
// if variables were expected but none were written (so an empty .env never
// deploys silently). Values are never printed.
func envWriteBlock(c Config, includePorts bool) string {
	var portLines []string
	if includePorts {
		for _, p := range c.Ports {
			if p.EnvVar != "" {
				portLines = append(portLines, fmt.Sprintf("%s=${%s}", p.EnvVar, p.EnvVar))
			}
		}
	}
	var b strings.Builder
	if len(portLines) > 0 {
		b.WriteString("cat > .env <<EOF\n")
		b.WriteString(strings.Join(portLines, "\n"))
		b.WriteString("\nEOF\n")
	} else {
		b.WriteString(": > .env\n")
	}
	b.WriteString(`written=0
for k in ${MOUNTABO_ENV_KEYS:-}; do
  v="${!k-}"
  if [ -z "$v" ]; then
    echo "warning: env var $k is empty (set it in the secrets tab, then redeploy)"
    continue
  fi
  printf '%s=%s\n' "$k" "$v" >> .env
  written=$((written + 1))
done
echo "wrote $written environment variable(s) to .env"
if [ -n "${MOUNTABO_ENV_KEYS:-}" ] && [ "$written" -eq 0 ]; then
  echo "error: environment variables were configured but none reached .env" >&2
  exit 1
fi
`)
	return b.String()
}

// workDirNoClone prepares a working directory for .env without cloning the repo:
// the docker strategy runs a self-contained image shipped from the runner, so it
// never needs the source on the box. It mirrors clonePull's sudo mkdir + chown
// (DEPLOY_DIR often lives under a root-owned path like /opt), minus the git half.
const workDirNoClone = `# Working directory for .env only (no repo checkout: the image is self-contained).
sudo mkdir -p "$DEPLOY_DIR"
sudo chown -R "$(id -un):$(id -gn)" "$(dirname "$DEPLOY_DIR")"
cd "$DEPLOY_DIR"`

// loadImageBlock loads the image tarball rsynced from the runner into the local
// docker store BEFORE anything touches the running container, so a truncated or
// corrupt transfer fails the deploy while the old container is still up. It
// captures the loaded tag(s) into $loaded (from `docker load`'s "Loaded image:"
// lines) for the later untag step, and installs a trap to remove the tarball on
// exit (success or failure). zstd/rsync are assumed present (modern distros ship
// them). The tarball path is the deployment's staging directory, matching the
// workflow's rsync target, so two deployments never overwrite each other's
// tarball mid-transfer.
//
// required=true (docker) treats a missing tarball as a hard error — the docker
// strategy has nothing to run without it. required=false (compose) tolerates a
// missing tarball: a pull-only compose stack ships none, and the server pulls
// every image on `up`, so $loaded is simply left empty.
func loadImageBlock(c Config, required bool) string {
	load := `loaded=$(zstd -dc "$TARBALL" | docker load | sed -n 's/^Loaded image: //p') || {
  echo "error: image load failed (corrupt or incomplete transfer?)" >&2
  exit 1
}
echo "loaded image(s): $loaded"`
	if !required {
		load = `loaded=""
if [ -f "$TARBALL" ]; then
  loaded=$(zstd -dc "$TARBALL" | docker load | sed -n 's/^Loaded image: //p') || {
    echo "error: image load failed (corrupt or incomplete transfer?)" >&2
    exit 1
  }
  echo "loaded image(s): $loaded"
else
  echo "no image tarball shipped; images will be pulled on the server"
fi`
	}
	return fmt.Sprintf(`TARBALL="%[1]s/image.tar.zst"
trap 'rm -f "$TARBALL"' EXIT
echo "Loading image from $TARBALL..."
%[2]s
`, StagingDir(c), load)
}

// healthCheckBlock probes the app's primary published port on the loopback after
// the swap, mirroring the monitor's probe (000 = curl could not connect). The
// container may still be starting, so it retries with a short backoff; if the app
// never answers it fails the deploy (the run goes red in Actions). Returns "" when
// no primary port is configured (nothing to probe). Note: same-host/same-port
// means there is a brief gap during the swap — load-before-swap is the safety we
// can guarantee; this is the liveness gate.
func healthCheckBlock(c Config) string {
	port := primaryHostPort(c)
	if port == "" {
		return ""
	}
	return fmt.Sprintf(`# Health check: confirm the app answers on its published port after the swap.
code=000
for i in 1 2 3 4 5; do
  code=$(curl -sS -k -o /dev/null --max-time 8 --connect-timeout 5 -w '%%{http_code}' http://127.0.0.1:%[1]s 2>/dev/null || echo 000)
  [ "$code" != "000" ] && break
  echo "waiting for app on :%[1]s (attempt $i)..."
  sleep 2
done
if [ "$code" = "000" ]; then
  echo "error: app did not respond on 127.0.0.1:%[1]s after deploy" >&2
  exit 1
fi
echo "app responded on :%[1]s (HTTP $code)"
`, port)
}

// untagOldImages bounds disk on the server by removing older mountabo image tags
// for this app, keeping the just-loaded one(s) in $loaded. `docker image prune`
// would not help here: these tags are named (one per commit sha), not dangling.
// referencePattern is a docker `reference=` filter (shell wildcards) scoping the
// removal to this app's images only. Best-effort: an image still in use (the one
// just started) is excluded via $loaded, and any rmi failure is swallowed.
func untagOldImages(referencePattern string) string {
	return fmt.Sprintf(`# Bound disk: drop older mountabo image tags for this app, keeping the loaded one(s).
if [ -n "$loaded" ]; then
  docker images --filter=reference=%[1]q --format '{{.Repository}}:{{.Tag}}' \
    | grep -vxF -f <(printf '%%s\n' "$loaded") \
    | xargs -r docker rmi -f 2>/dev/null || true
fi
`, referencePattern)
}

// composeImageOverride copies the runner-generated image override (rsynced next
// to the tarball) into the app directory and merges it into COMPOSE_ARGS LAST, so
// its `image:` pins win over anything in the user's compose/override and every
// build-service resolves to the tag just loaded (never rebuilt on the server).
func composeImageOverride(c Config) string {
	return fmt.Sprintf(`# Pin build-services to the runner-built images (loaded above); merged last so
# its image: wins and the server never rebuilds.
if [ -f "%[1]s/mountabo.image.override.yml" ]; then
  cp "%[1]s/mountabo.image.override.yml" ./mountabo.image.override.yml
  COMPOSE_ARGS="$COMPOSE_ARGS -f mountabo.image.override.yml"
fi

`, StagingDir(c))
}

// primaryHostPort is the app's main published host port (the first configured
// port with a non-empty host value), used by the in-script health check. "" when
// none is configured.
func primaryHostPort(c Config) string {
	for _, p := range c.Ports {
		if v := strings.TrimSpace(hostValue(p)); v != "" {
			return v
		}
	}
	return ""
}

func composeScript(c Config) string {
	var b strings.Builder
	b.WriteString(header(c))
	b.WriteByte('\n')
	b.WriteString(lockPreamble(c))

	var exports []string
	for _, p := range c.Ports {
		if p.EnvVar != "" {
			exports = append(exports, fmt.Sprintf("export %s=%s", p.EnvVar, hostValue(p)))
		}
	}
	if len(exports) > 0 {
		b.WriteString("\n# Ports for this environment (detected from your compose file).\n")
		b.WriteString(strings.Join(exports, "\n"))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	b.WriteString(gitAuth(c))
	b.WriteString(clonePull)
	b.WriteString("\n\n")
	b.WriteString(cdRoot(c))
	// Pin the compose project name so each (app, branch) deploy owns a stable,
	// known set of containers (compose otherwise defaults the project name to
	// the deploy directory basename, which makes the delete teardown unable to
	// find the containers by app name).
	// The compose project is the deployment id. It used to be "<app>-<branch>",
	// which two environments of one repo on the same branch shared — one compose
	// project, one set of containers, one set of project-scoped volumes. Labels
	// carry the same identity onto every container compose creates, so teardown
	// can select this deployment's resources by ownership instead of by name.
	fmt.Fprintf(&b, "export COMPOSE_PROJECT_NAME=%q\n", c.deploymentID())
	b.WriteByte('\n')
	b.WriteString("# Write .env (ports + your environment variables) for docker compose.\n")
	b.WriteString(envWriteBlock(c, true))
	b.WriteString("\n")
	b.WriteString(populateEnvFiles)
	b.WriteString(composeEnvOverride(c))
	b.WriteString(composeImageOverride(c))
	b.WriteString(loadImageBlock(c, false))
	b.WriteString("\n")
	b.WriteString(`echo "Swapping to new containers (built on the runner, no build here)..."
docker compose $COMPOSE_ARGS up -d --no-build --force-recreate --remove-orphans

docker compose $COMPOSE_ARGS ps
`)
	b.WriteString("\n")
	b.WriteString(healthCheckBlock(c))
	if primaryHostPort(c) != "" {
		b.WriteString("\n")
	}
	b.WriteString(untagOldImages(c.imageRepo() + "-*:*"))
	b.WriteString(`echo "=== Deployment complete ==="
`)
	return b.String()
}

func dockerScript(c Config) string {
	var b strings.Builder
	b.WriteString(header(c))
	b.WriteByte('\n')
	b.WriteString(lockPreamble(c))
	b.WriteByte('\n')
	b.WriteString(workDirNoClone)
	b.WriteString("\n\n")
	b.WriteString(loadImageBlock(c, true))
	b.WriteString("\n")
	b.WriteString("# Write .env (your environment variables) for the container.\n")
	b.WriteString(envWriteBlock(c, false))
	b.WriteString("\n")

	// The exact image tag the runner built + loaded is forwarded as MOUNTABO_IMAGE;
	// running it (not <app>:$BRANCH) guarantees we run precisely what was shipped.
	b.WriteString("IMAGE=\"${MOUNTABO_IMAGE:?MOUNTABO_IMAGE must be set by the workflow}\"\n")
	// The container is named for the deployment, not "<app>-<branch>": two
	// environments of one app on the same branch previously produced the same
	// container name, so whichever deployed last replaced the other's container.
	fmt.Fprintf(&b, "CONTAINER=%q\n\n", c.deploymentID())
	b.WriteString(`echo "Swapping container..."
docker rm -f "$CONTAINER" 2>/dev/null || true
docker run -d --name "$CONTAINER" --restart unless-stopped \
  `)
	for _, l := range c.ownershipLabelArgs() {
		fmt.Fprintf(&b, "--label %s \\\n  ", l)
	}
	b.WriteString(`--env-file .env`)
	for _, p := range c.Ports {
		if p.Container != "" {
			fmt.Fprintf(&b, " \\\n  -p %s:%s", hostValue(p), p.Container)
		}
	}
	b.WriteString(" \\\n  \"$IMAGE\"\n")
	b.WriteString("docker ps --filter \"name=$CONTAINER\"\n")
	b.WriteString("\n")
	b.WriteString(healthCheckBlock(c))
	if primaryHostPort(c) != "" {
		b.WriteString("\n")
	}
	b.WriteString(untagOldImages(c.imageRepo() + ":*"))
	b.WriteString(`echo "=== Deployment complete ==="
`)
	return b.String()
}

// hostValue is the host port to publish: the operator's chosen value, falling
// back to the container port when they left it blank.
func hostValue(p Port) string {
	if v := strings.TrimSpace(p.Value); v != "" {
		return v
	}
	return p.Container
}
