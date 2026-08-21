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
}

// DeployScriptPath is where deploy.sh is committed (repo root). The workflow's
// scp step copies it from here by name.
const DeployScriptPath = "deploy.sh"

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

// Workflow renders the GitHub Actions workflow YAML. It is the same for both
// strategies, it only copies deploy.sh to the server and runs it; the strategy
// lives entirely in deploy.sh. The passthrough env block and envs list cover
// the operator's env vars plus DEPLOY_DIR (all from secrets); the job pins the
// deployment environment so its secrets resolve. It also passes
// MOUNTABO_ENV_KEYS (the operator's env-var names) so deploy.sh can assemble
// .env from the forwarded secrets without the names being baked into deploy.sh
// — meaning a secret added later only needs this workflow regenerated, not the
// script.
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

	forward := strings.Join(append(append([]string{}, names...), "MOUNTABO_ENV_KEYS"), ",")

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

      - name: Copy deploy script to server
        uses: appleboy/scp-action@v0.1.7
        with:
          host: ${{ secrets.SERVER_HOST }}
          username: ${{ secrets.SERVER_USER }}
          key: ${{ secrets.SERVER_SSH_KEY }}
          source: "deploy.sh"
          target: "/tmp/%[1]s-deploy"

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
            chmod +x /tmp/%[1]s-deploy/deploy.sh
            /tmp/%[1]s-deploy/deploy.sh %[2]s
`, c.App, c.Branch, c.environment(), envBlock.String(), forward)
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
DEPLOY_DIR="${DEPLOY_DIR:-%[2]s}/$BRANCH"
REPO_URL="git@github.com:%[3]s/%[4]s.git"

echo "=== %[5]s deploy (branch: $BRANCH) ==="`, c.Branch, c.DeployDir, c.Owner, c.Repo, c.App)
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
const composeEnvOverride = `# Mount the operator's .env into every compose service as container environment
# (compose otherwise uses .env only for interpolation, not as container env).
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
        printf '  %s:\n    env_file:\n      - .env\n' "$s"
      done
    } > mountabo.env.override.yml
    COMPOSE_ARGS="$COMPOSE_ARGS -f mountabo.env.override.yml"
    echo "mounting .env into services: $services"
  fi
fi

`

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

func composeScript(c Config) string {
	var b strings.Builder
	b.WriteString(header(c))
	b.WriteByte('\n')

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
	fmt.Fprintf(&b, "export COMPOSE_PROJECT_NAME='%s-%s'\n\n", c.App, c.Branch)
	b.WriteString("# Write .env (ports + your environment variables) for docker compose.\n")
	b.WriteString(envWriteBlock(c, true))
	b.WriteString("\n")
	b.WriteString(populateEnvFiles)
	b.WriteString(composeEnvOverride)
	b.WriteString(`# Zero-downtime: build new images while old containers keep running, then swap.
echo "Building images..."
docker compose $COMPOSE_ARGS build
echo "Swapping to new containers..."
docker compose $COMPOSE_ARGS up -d --force-recreate --remove-orphans

docker compose $COMPOSE_ARGS ps
echo "=== Deployment complete ==="
`)
	return b.String()
}

func dockerScript(c Config) string {
	var b strings.Builder
	b.WriteString(header(c))
	b.WriteString("\n\n")
	b.WriteString(gitAuth(c))
	b.WriteString(clonePull)
	b.WriteString("\n\n")
	b.WriteString(cdRoot(c))
	b.WriteString("# Write .env (your environment variables) for the container.\n")
	b.WriteString(envWriteBlock(c, false))
	b.WriteString("\n")

	fmt.Fprintf(&b, "IMAGE=%q\n", c.App+":$BRANCH")
	fmt.Fprintf(&b, "CONTAINER=%q\n\n", c.App+"-$BRANCH")
	b.WriteString(`echo "Building image..."
docker build -t "$IMAGE" .
echo "Swapping container..."
docker rm -f "$CONTAINER" 2>/dev/null || true
docker run -d --name "$CONTAINER" --restart unless-stopped \
  --env-file .env`)
	for _, p := range c.Ports {
		if p.Container != "" {
			fmt.Fprintf(&b, " \\\n  -p %s:%s", hostValue(p), p.Container)
		}
	}
	b.WriteString(" \\\n  \"$IMAGE\"\n")
	b.WriteString(`docker ps --filter "name=$CONTAINER"
echo "=== Deployment complete ==="
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
