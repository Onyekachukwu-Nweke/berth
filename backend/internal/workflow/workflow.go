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
	// Hooks are the operator's release hooks, already validated and in phase order.
	Hooks []Hook
	// HooksVersion fingerprints the hook set. An approval names one artifact
	// running one specific set of commands, so editing a command after approving
	// changes this and invalidates the approval — which is the point.
	HooksVersion string
	// ComposeFile is an explicit path to the stack's compose file, for projects
	// that do not use one of the four conventional names (docker-compose.prod.yml
	// being the common case). Empty falls back to those names.
	ComposeFile string
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

	// Release identity, forwarded so the script can record what it deployed.
	// MOUNTABO_ARTIFACT_ID names the build (not the commit — see ArtifactIDExpr);
	// MOUNTABO_OPERATION_ID names this run so re-reading it is idempotent.
	fmt.Fprintf(&envBlock, "\n          MOUNTABO_ARTIFACT_ID: %s", ArtifactIDExpr)
	fmt.Fprintf(&envBlock, "\n          MOUNTABO_OPERATION_ID: %s", OperationIDExpr)
	fmt.Fprintf(&envBlock, "\n          %s", commitEnvLine())
	forwardNames := append(append([]string{}, names...),
		"MOUNTABO_ENV_KEYS", "MOUNTABO_ARTIFACT_ID", "MOUNTABO_OPERATION_ID", "MOUNTABO_COMMIT")
	// The docker strategy runs one known image tag; forward it so deploy.sh runs
	// exactly what the runner built and loaded. Compose gets the mapping from the
	// image-override file instead, so it needs no extra env.
	if c.strategy() == Docker {
		fmt.Fprintf(&envBlock, "\n          MOUNTABO_IMAGE: %s:%s", c.imageRepo(), ArtifactIDExpr)
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

%[11]s%[6]s
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
		StagingDir(c), ScriptPath(c), ScriptName(c), nameBuildStep)
}

// buildStep renders the runner build+save step for the strategy: it builds the
// image(s) and writes image.tar.zst next to the deploy script so the transfer
// step can ship them.
//
// The tag is the ARTIFACT id, not the commit sha. Keying it on the sha meant the
// same commit rebuilt with different build arguments, a moved base image or newly
// resolved dependencies overwrote the earlier image in place — so the artifact
// that had been running was no longer recoverable. A secret change triggers
// exactly such a rebuild.
func buildStep(c Config) string {
	root := rootDirOrDot(c)
	if c.strategy() == Docker {
		// Single image from the repo's Dockerfile; context is the app root.
		return fmt.Sprintf(`      - name: Build image on the runner
        run: |
          docker build -t %[1]s:%[3]s %[2]s
          docker save %[1]s:%[3]s | zstd -T0 -o image.tar.zst
`, c.imageRepo(), root, ArtifactIDExpr)
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
            tag="%[1]s-$svc:%[3]s"
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
`, c.imageRepo(), root, ArtifactIDExpr)
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
	if root := appRoot(c); root != "" {
		return root
	}
	return "."
}

// appRoot is the app's directory relative to the repo root, or "" at the root.
//
// It strips one leading "./" and any surrounding slashes — and nothing else. The
// previous implementation used strings.TrimLeft with the cutset "./", which
// removes EVERY leading "." and "/" character, so a legitimate dotted directory
// like ".config/app" silently became "config/app": the runner built from the
// wrong directory, or failed to find one at all.
func appRoot(c Config) string {
	root := strings.Trim(strings.TrimPrefix(strings.TrimSpace(c.RootDir), "./"), "/")
	if root == "." {
		return ""
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

// populateEnvFiles regenerates every per-service env file a compose declares
// (env_file:) from the .env just written, so configured variables reach every
// service. Only paths ending in .env are touched, so unrelated list items (ports,
// volumes) are ignored. Paths are relative to the compose file in the current
// directory, matching how compose resolves env_file.
//
// It used to write each file ONLY when it was missing ("[ -f "$ef" ] || cp"),
// which is the single biggest reason a changed secret never reached a running
// container. These files are untracked, and the deploy resets the tree with
// "git reset --hard", which leaves untracked files alone — so a file created by
// the first deploy survived every later one, pinning that service to the values
// it had on day one. The root .env looked correct the whole time, which is what
// made it so hard to see.
//
// Three cases have to be distinguished, and getting any of them wrong
// reintroduces a bug:
//
//   - env_file pointing AT the project .env: copying a file onto itself fails,
//     and appending it to itself would double every line. Skipped outright.
//   - a COMMITTED env file: restored from git first so its defaults come back,
//     then the operator's values appended after them. Later assignments win in
//     both dotenv and docker --env-file, so the operator overrides the default
//     rather than the other way round.
//   - an UNTRACKED file: it is ours from a previous deploy, so it is replaced.
//
// The trailing-newline check matters: appending to a committed file that does
// not end in a newline would fuse its last entry with the first appended one
// into a single malformed line.
const populateEnvFiles = `# Regenerate any per-service env files the compose references (env_file:) from
# the .env above, so a changed secret reaches every service and not just the root.
mountabo_write_service_env() {
  ef="$1"
  if [ "$(readlink -f "$ef" 2>/dev/null || echo "$ef")" = "$(readlink -f .env)" ]; then
    return 0
  fi
  mkdir -p "$(dirname "$ef")"
  if git ls-files --error-unmatch -- "$ef" >/dev/null 2>&1; then
    git checkout -- "$ef"
    if [ -s "$ef" ] && [ "$(tail -c1 "$ef" | wc -l)" -eq 0 ]; then
      printf '\n' >> "$ef"
    fi
    cat .env >> "$ef"
    echo "refreshed $ef (committed defaults + your configured values)"
  else
    cp .env "$ef"
    echo "refreshed $ef"
  fi
}

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
    *.env) mountabo_write_service_env "$ef" ;;
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
// composeEnvOverride mounts the operator's .env into every compose service as
// container environment. Docker Compose reads a project .env only for ${VAR}
// interpolation in the compose file, NOT as container env — so a service that
// declares no env_file:/environment: never receives the operator's secrets. This
// generates a mountabo-owned override that attaches "env_file: .env" and this
// deployment's ownership labels to each service, merged in with -f. It reads only
// service NAMES; the user's compose is never parsed or edited.
//
// Two failure modes are fixed here, both of which produced a green deploy with no
// configuration in it:
//
//   - Service discovery swallowed every error ("2>/dev/null || true"). When it
//     produced nothing, no override was written and NOT ONE variable reached ANY
//     service — silently, with the deploy reporting success. It now captures the
//     compose error, prints it, and fails the run rather than shipping an
//     unconfigured app.
//   - The compose file was matched against four hardcoded names, so a project
//     using docker-compose.prod.yml was never found. The file is now configurable
//     (Config.ComposeFile), falling back to those names.
//
// Services are read from "config --format json" via jq rather than inferred from
// YAML text: anchors, extends and multiple -f files all normalise there, and jq
// is guaranteed by bootstrap. Older compose builds without --format fall back to
// "config --services". --no-interpolate matches the runner's build step: only the
// structure is needed here, and a compose file referencing a variable that is
// only defined at runtime must not fail discovery.
func composeEnvOverride(c Config) string {
	var candidates []string
	if f := strings.TrimSpace(c.ComposeFile); f != "" {
		candidates = append(candidates, f)
	}
	candidates = append(candidates, "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml")

	// Two renderings: quoted for the shell loop (a path may contain a space), and
	// a plain list for the error message, which is itself inside a double-quoted
	// echo and so cannot carry embedded quotes.
	quoted := make([]string, 0, len(candidates))
	for _, f := range candidates {
		quoted = append(quoted, fmt.Sprintf("%q", f))
	}

	return fmt.Sprintf(`# Mount the operator's .env into every compose service as container environment
# (compose otherwise uses .env only for interpolation, not as container env), and
# stamp this deployment's ownership labels so teardown can select them.
MOUNTABO_LABELS=%[3]q
COMPOSE=""
for cf in %[1]s; do
  if [ -f "$cf" ]; then COMPOSE="$cf"; break; fi
done
if [ -z "$COMPOSE" ]; then
  echo "error: no compose file found (looked for: %[2]s)" >&2
  echo "       set the compose file for this environment in mountabo if yours is named differently" >&2
  exit 1
fi

COMPOSE_ARGS="-f $COMPOSE"
for ov in docker-compose.override.yml docker-compose.override.yaml compose.override.yml compose.override.yaml; do
  if [ -f "$ov" ]; then COMPOSE_ARGS="$COMPOSE_ARGS -f $ov"; break; fi
done

# Enumerate services, keeping the error if it fails. A stack whose services
# cannot be listed cannot have configuration injected into it, and shipping that
# silently is exactly what this replaces.
compose_err="$(mktemp)"
services="$(docker compose $COMPOSE_ARGS config --no-interpolate --format json 2>"$compose_err" | jq -r '.services | keys[]' 2>/dev/null || true)"
if [ -z "$services" ]; then
  # Older compose plugins have no --format; fall back before treating it as fatal.
  services="$(docker compose $COMPOSE_ARGS config --services 2>>"$compose_err" || true)"
fi
if [ -z "$services" ]; then
  echo "error: could not read the services from $COMPOSE, so no environment variables can be injected." >&2
  echo "       docker compose reported:" >&2
  sed 's/^/       /' "$compose_err" >&2 || true
  rm -f "$compose_err"
  exit 1
fi
rm -f "$compose_err"

{
  echo "services:"
  for s in $services; do
    printf '  %%s:\n    env_file:\n      - .env\n    labels:\n' "$s"
    for l in $MOUNTABO_LABELS; do printf '      - %%s\n' "$l"; done
  done
} > mountabo.env.override.yml
COMPOSE_ARGS="$COMPOSE_ARGS -f mountabo.env.override.yml"
echo "mounting .env into services: $(echo $services | tr '\n' ' ')"

`, strings.Join(quoted, " "), strings.Join(candidates, ", "), c.allLabels())
}

// cdRoot is the `cd <rootDir>` line (with trailing newline) when the app lives
// in a sub-directory, or "" for the repo root.
func cdRoot(c Config) string {
	root := appRoot(c)
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
  # Test whether the variable is SET, not whether it is non-empty. A variable an
  # operator deliberately set to "" is a configured value: dropping it made the
  # application see an unset variable instead of an empty one, which for most
  # runtimes is a different thing entirely (undefined vs ""). Only a name that
  # never arrived — not configured on the GitHub environment, or not forwarded —
  # is skipped.
  if [ -z "${!k+x}" ]; then
    echo "warning: env var $k is not set on this GitHub environment (add it in the secrets tab, then redeploy)"
    continue
  fi
  printf '%s=%s\n' "$k" "${!k}" >> .env
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
loaded=""
# A rollback re-points to images already in the local store — there is no
# tarball, and re-loading one would defeat the point of not rebuilding.
if [ "$MOUNTABO_MODE" = deploy ]; then
echo "Loading image from $TARBALL..."
%[2]s
else
  loaded="$(jq -r '.images[]?.ref // empty' "$BUNDLE/manifest.json" 2>/dev/null | tr '\n' ' ')"
  echo "re-pointing to retained image(s): $loaded"
fi
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

// composeImageOverride copies the runner-generated image override (rsynced next
// to the tarball) into the app directory and merges it into COMPOSE_ARGS LAST, so
// its `image:` pins win over anything in the user's compose/override and every
// build-service resolves to the tag just loaded (never rebuilt on the server).
func composeImageOverride(c Config) string {
	return fmt.Sprintf(`# Pin build-services to exact images; merged last so its image: wins and the
# server never rebuilds. On a deploy the pins come from the runner (shipped
# beside the tarball); on a rollback they come from the target release's own
# bundle, which is what makes the rollback re-point rather than rebuild.
if [ "$MOUNTABO_MODE" = rollback ]; then
  if [ ! -f "$BUNDLE/image.override.yml" ]; then
    echo "error: release $ARTIFACT_ID has no image pins recorded, so it cannot be rolled back to" >&2
    exit 1
  fi
  cp "$BUNDLE/image.override.yml" ./mountabo.image.override.yml
  COMPOSE_ARGS="$COMPOSE_ARGS -f mountabo.image.override.yml"
elif [ -f "%[1]s/mountabo.image.override.yml" ]; then
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
	b.WriteString(rollbackKindBlock)
	b.WriteString(artifactIdentityBlock)

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
	// A rollback re-points to a retained artifact; it must not move the working
	// tree, because the checkout is what the release's own compose was rendered
	// against. Fetching would also require the branch to still resolve.
	b.WriteString("if [ \"$MOUNTABO_MODE\" = deploy ]; then\n")
	b.WriteString(indentShell(clonePull))
	b.WriteString("\nfi\n")
	b.WriteString(`cd "$DEPLOY_DIR"` + "\n")
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
	// Secrets are write-only on GitHub, so mountabo cannot reconstruct .env for a
	// rollback. The file already on the box is the deployment's current
	// configuration and is reused as-is — a rollback restores code, not secrets.
	b.WriteString("if [ \"$MOUNTABO_MODE\" = deploy ]; then\n")
	b.WriteString("# Write .env (ports + your environment variables) for docker compose.\n")
	b.WriteString(indentShell(envWriteBlock(c, true)))
	b.WriteString("\nfi\n\n")
	b.WriteString(populateEnvFiles)
	b.WriteString(composeEnvOverride(c))
	b.WriteString(composeImageOverride(c))
	b.WriteString(loadImageBlock(c, false))
	b.WriteString(stateHelpers)
	b.WriteString(hookContext(c))
	b.WriteString(HookDispatcher(c.Hooks))
	// The recovery record is complete while the previous release is still
	// serving; only then is the swap attempted.
	b.WriteString(bundleBlock(c))
	b.WriteString("\n")
	b.WriteString(HookPhaseCall(c.Hooks, HookPostBuild))
	b.WriteString(migrationPhase(c))
	b.WriteString(HookPhaseCall(c.Hooks, HookPreDeploy))
	b.WriteString(`echo "Swapping to new containers (built on the runner, no build here)..."
# --remove-orphans on a deploy: the stack is the compose file, so a service the
# operator deleted should go. It is deliberately NOT passed on a rollback — a
# service that exists today but not in the target release would be stopped
# silently, which is a destructive surprise rather than a rollback.
if [ "$MOUNTABO_MODE" = rollback ]; then
  docker compose $COMPOSE_ARGS up -d --no-build --force-recreate
else
  docker compose $COMPOSE_ARGS up -d --no-build --force-recreate --remove-orphans
fi

docker compose $COMPOSE_ARGS ps
`)
	b.WriteString("\n")
	b.WriteString(HookPhaseCall(c.Hooks, HookPostDeploy))
	b.WriteString(healthCheckBlock(c))
	if primaryHostPort(c) != "" {
		b.WriteString("\n")
	}
	b.WriteString(HookPhaseCall(c.Hooks, HookHealthCheck))
	b.WriteString(rollbackHookPhase(c))
	b.WriteString(verifyAndPromoteBlock(c))
	b.WriteString(pruneBlock)
	b.WriteString(`echo "=== Deployment complete ==="
`)
	return b.String()
}

func dockerScript(c Config) string {
	var b strings.Builder
	b.WriteString(header(c))
	b.WriteByte('\n')
	b.WriteString(lockPreamble(c))
	b.WriteString(rollbackKindBlock)
	b.WriteString(artifactIdentityBlock)
	b.WriteByte('\n')
	b.WriteString(workDirNoClone)
	b.WriteString("\n\n")
	b.WriteString(loadImageBlock(c, true))
	b.WriteString("\n")
	// See composeScript: a rollback reuses the .env already on the box, because
	// GitHub Actions secret values cannot be read back to rebuild it.
	b.WriteString("if [ \"$MOUNTABO_MODE\" = deploy ]; then\n")
	b.WriteString("# Write .env (your environment variables) for the container.\n")
	b.WriteString(indentShell(envWriteBlock(c, false)))
	b.WriteString("\nfi\n\n")
	b.WriteString(stateHelpers)
	b.WriteString(hookContext(c))
	b.WriteString(HookDispatcher(c.Hooks))
	// The recovery record is complete while the previous container is still
	// running; only then is it replaced.
	b.WriteString(bundleBlock(c))
	b.WriteString("\n")

	// The exact image tag the runner built + loaded is forwarded as MOUNTABO_IMAGE;
	// running it (not <app>:$BRANCH) guarantees we run precisely what was shipped.
	// On a deploy the workflow forwards the exact tag it built and shipped; on a
	// rollback the tag comes from the target release's own manifest, so the
	// container starts the same image that release ran rather than anything
	// resolved fresh.
	b.WriteString(`if [ "$MOUNTABO_MODE" = rollback ]; then
  IMAGE="$(jq -r '.images[0].ref // empty' "$BUNDLE/manifest.json" 2>/dev/null)"
  if [ -z "$IMAGE" ]; then
    echo "error: release $ARTIFACT_ID has no image recorded, so it cannot be rolled back to" >&2
    exit 1
  fi
else
  IMAGE="${MOUNTABO_IMAGE:?MOUNTABO_IMAGE must be set by the workflow}"
fi
`)
	// The container is named for the deployment, not "<app>-<branch>": two
	// environments of one app on the same branch previously produced the same
	// container name, so whichever deployed last replaced the other's container.
	fmt.Fprintf(&b, "CONTAINER=%q\n\n", c.deploymentID())
	b.WriteString(HookPhaseCall(c.Hooks, HookPostBuild))
	b.WriteString(migrationPhase(c))
	b.WriteString(HookPhaseCall(c.Hooks, HookPreDeploy))
	b.WriteString(`echo "Swapping container..."
docker rm -f "$CONTAINER" 2>/dev/null || true
docker run -d --name "$CONTAINER" --restart unless-stopped \
  `)
	for _, l := range c.artifactLabelArgs() {
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
	b.WriteString(HookPhaseCall(c.Hooks, HookPostDeploy))
	b.WriteString(healthCheckBlock(c))
	if primaryHostPort(c) != "" {
		b.WriteString("\n")
	}
	b.WriteString(HookPhaseCall(c.Hooks, HookHealthCheck))
	b.WriteString(rollbackHookPhase(c))
	b.WriteString(verifyAndPromoteBlock(c))
	b.WriteString(pruneBlock)
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
