package workflow

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// testID is a stand-in for a real usecase.DeriveDeploymentID result. Every
// server-side name derives from it, so the assertions below read in terms of it
// rather than the app or branch.
const testID = "mtb-aaaaaaaaaaaa"

func base() Config {
	return Config{
		App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		RootDir: "./apps/web", DeployDir: "/opt/shop",
		DeploymentID: testID,
		Ports:        []Port{{EnvVar: "FRONTEND_PORT", Value: "3000", Container: "3000"}},
		EnvVars:      []EnvVar{{Key: "DATABASE_URL", Value: "x"}},
	}
}

func TestDeployScript_Compose(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)
	for _, want := range []string{"docker load", "docker compose $COMPOSE_ARGS up -d --no-build", "export FRONTEND_PORT=3000", `cd "apps/web"`} {
		if !strings.Contains(s, want) {
			t.Errorf("compose script missing %q", want)
		}
	}
	// The image is built on the runner and loaded here, never built on the server.
	for _, notWant := range []string{"docker run", "docker compose $COMPOSE_ARGS build"} {
		if strings.Contains(s, notWant) {
			t.Errorf("compose script should not contain %q", notWant)
		}
	}
}

func TestDeployScript_ComposeMountsEnvIntoServices(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)
	// Enumerates services and writes a mountabo-owned env_file override, then runs
	// compose with it merged in — so operator secrets reach every container even
	// when the user's compose declares no env_file:/environment:.
	for _, want := range []string{
		"COMPOSE_ARGS=\"-f $COMPOSE\"",
		"docker-compose.override.yml",
		"COMPOSE_ARGS=\"$COMPOSE_ARGS -f $ov\"",
		"docker compose $COMPOSE_ARGS config --services",
		"mountabo.env.override.yml",
		"env_file:",
		"docker compose $COMPOSE_ARGS up -d --no-build",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("compose script missing %q:\n%s", want, s)
		}
	}
	// The user's compose is never edited in place.
	if strings.Contains(s, "yq ") || strings.Contains(s, "sed -i") {
		t.Errorf("compose script should not edit the user's compose in place:\n%s", s)
	}
}

func TestDeployScript_Docker(t *testing.T) {
	c := base()
	c.Strategy = Docker
	s := DeployScript(c)
	for _, want := range []string{
		"docker load",
		`IMAGE="${MOUNTABO_IMAGE`,
		"docker run -d",
		"-p 3000:3000",
		`--restart unless-stopped`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("docker script missing %q", want)
		}
	}
	if strings.Contains(s, "docker compose") {
		t.Error("docker script should not use docker compose")
	}
}

// TestDockerScript_OffloadShape guards the build-offload contract for the docker
// strategy: it loads a runner-built tarball, cleans it up on exit, health-checks
// the app, and bounds disk by untagging older images — and it never builds or
// clones on the server.
func TestDockerScript_OffloadShape(t *testing.T) {
	c := base()
	c.Strategy = Docker
	s := DeployScript(c)
	for _, want := range []string{
		"zstd -dc",
		"docker load",
		`trap 'rm -f "$TARBALL"' EXIT`,
		"curl -sS -k",           // health check probe
		"http://127.0.0.1:3000", // primary port
		// Retention is driven by the release pointers, not by "keep only the tag
		// just loaded" — that untagged the previous release's image and left
		// rollback with nothing to point at.
		"mountabo_prune_releases",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("docker offload script missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"docker build", "git clone", "git fetch", "clonePull"} {
		if strings.Contains(s, bad) {
			t.Errorf("docker offload script should not contain %q", bad)
		}
	}
}

// TestComposeScript_OffloadShape guards the same contract for compose: load the
// runner-built images, merge the image override LAST (so its image: wins), run
// with --no-build, and untag older per-app images.
func TestComposeScript_OffloadShape(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)
	for _, want := range []string{
		"docker load",
		"docker compose $COMPOSE_ARGS up -d --no-build",
		"mountabo.image.override.yml",
		"mountabo_prune_releases",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("compose offload script missing %q:\n%s", want, s)
		}
	}
	// The image override must be merged after the env override so its image: wins.
	if envIdx, imgIdx := strings.Index(s, "-f mountabo.env.override.yml"), strings.LastIndex(s, "-f mountabo.image.override.yml"); envIdx < 0 || imgIdx < 0 || envIdx > imgIdx {
		t.Errorf("image override must be merged after env override (env=%d img=%d)", envIdx, imgIdx)
	}
}

func TestWorkflow_ForwardsEnvKeyList(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "DATABASE_URL"}, {Key: "API_KEY"}}
	w := Workflow(c)
	// The operator names are passed as a space-separated list so deploy.sh can
	// assemble .env without the names being baked into the script...
	if !strings.Contains(w, `MOUNTABO_ENV_KEYS: "DATABASE_URL API_KEY"`) {
		t.Errorf("workflow should pass MOUNTABO_ENV_KEYS with the operator names:\n%s", w)
	}
	// ...and MOUNTABO_ENV_KEYS is itself forwarded over SSH.
	if !strings.Contains(w, ",MOUNTABO_ENV_KEYS") {
		t.Errorf("workflow envs must forward MOUNTABO_ENV_KEYS:\n%s", w)
	}
}

func TestDeployScript_EnvIsNameAgnostic(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "DATABASE_URL"}}
	s := DeployScript(c)
	// The .env is built by looping the forwarded key list, not baked per name.
	if !strings.Contains(s, "for k in ${MOUNTABO_ENV_KEYS:-}; do") {
		t.Errorf("deploy.sh should loop MOUNTABO_ENV_KEYS to write .env:\n%s", s)
	}
	if strings.Contains(s, "DATABASE_URL=${DATABASE_URL}") {
		t.Errorf("deploy.sh must not bake env-var names into .env:\n%s", s)
	}
	// Verification: reports the count and fails an empty .env when keys were expected.
	for _, want := range []string{"wrote $written environment variable(s) to .env", "is not set on this GitHub environment", "exit 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy.sh missing verification %q:\n%s", want, s)
		}
	}
	// Ports remain baked (they are not secrets).
	if !strings.Contains(s, "FRONTEND_PORT=${FRONTEND_PORT}") {
		t.Errorf("compose .env should still seed baked port vars:\n%s", s)
	}
}

// TestWorkflow_DockerBuildsAndShips guards that the docker workflow builds the
// image on the runner, saves it to a zstd tarball, rsyncs it (not scp) to the
// box, and forwards the exact image tag for the server to run.
func TestWorkflow_DockerBuildsAndShips(t *testing.T) {
	c := base()
	c.Strategy = Docker
	w := Workflow(c)
	for _, want := range []string{
		"docker build -t mountabo/" + testID + ":" + ArtifactIDExpr,
		"docker save mountabo/" + testID + ":" + ArtifactIDExpr + " | zstd",
		"rsync -a --partial",
		`srcs=".mountabo/deploy-` + testID + `.sh"`,
		"for f in image.tar.zst; do",
		"MOUNTABO_IMAGE: mountabo/" + testID + ":" + ArtifactIDExpr,
		",MOUNTABO_IMAGE", // forwarded over SSH
	} {
		if !strings.Contains(w, want) {
			t.Errorf("docker workflow missing %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "appleboy/scp-action") {
		t.Errorf("scp-action should be replaced by rsync:\n%s", w)
	}
}

// TestWorkflow_ComposeBuildsImagesAndShipsOverride guards that the compose
// workflow enumerates build-services on the runner, builds + saves their images
// to a tarball, and ships the generated image override alongside it — with no
// MOUNTABO_IMAGE (compose resolves tags via the override, not a single tag).
func TestWorkflow_ComposeBuildsImagesAndShipsOverride(t *testing.T) {
	c := base()
	c.Strategy = Compose
	w := Workflow(c)
	for _, want := range []string{
		`docker compose -f "$compose" config --no-interpolate --format json`,
		"mountabo.image.override.yml",
		`tag="mountabo/` + testID + `-$svc:` + ArtifactIDExpr + `"`,
		"docker save $tags | zstd",
		"apps/web/image.tar.zst", // rsync source carries the app-root prefix
		"apps/web/mountabo.image.override.yml",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("compose workflow missing %q:\n%s", want, w)
		}
	}
	if strings.Contains(w, "MOUNTABO_IMAGE") {
		t.Errorf("compose workflow should not forward MOUNTABO_IMAGE:\n%s", w)
	}
}

func TestWorkflow_PinsEnvironmentAndSecrets(t *testing.T) {
	c := base()
	c.Environment = "production"
	w := Workflow(c)
	for _, want := range []string{"environment: production", "DATABASE_URL: ${{ secrets.DATABASE_URL }}", "envs: DATABASE_URL,DEPLOY_DIR", ScriptName(base()) + " main"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
}

func TestWorkflow_EnvironmentDefaultsToBranch(t *testing.T) {
	if !strings.Contains(Workflow(base()), "environment: main") {
		t.Error("environment should default to the branch")
	}
}

// TestWorkflow_Triggers guards the run-trigger contract: the branch push trigger
// is the SOLE source of automatic deploy runs (the deploy flow no longer calls
// workflow_dispatch, to avoid a duplicate run), and workflow_dispatch stays for
// manual re-runs. A future edit dropping either must fail here.
func TestWorkflow_Triggers(t *testing.T) {
	w := Workflow(base())
	for _, want := range []string{"on:", "push:", "branches:", "- main", "workflow_dispatch:"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow triggers missing %q", want)
		}
	}
}

// TestWorkflow_ConcurrencySerializes guards that overlapping runs for the same
// target serialize (queue) rather than cancel — a per-(app,branch) group with
// cancel-in-progress:false. Never interrupt a half-applied deploy.
func TestWorkflow_ConcurrencySerializes(t *testing.T) {
	w := Workflow(base())
	for _, want := range []string{"concurrency:", "group: mountabo-deploy-shop-main", "cancel-in-progress: false"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow concurrency missing %q", want)
		}
	}
}

// TestAppRoot_StripsOnlyWhatItShould pins a real corruption. The previous
// implementation used strings.TrimLeft with the cutset "./", which strips every
// leading "." and "/" character rather than one "./" prefix — so a dotted
// directory had its name silently rewritten and the runner built from the wrong
// place. It reached both the runner's build context and the script's cd.
func TestAppRoot_StripsOnlyWhatItShould(t *testing.T) {
	for in, want := range map[string]string{
		"./apps/web":  "apps/web",
		"apps/web":    "apps/web",
		"/apps/web/":  "apps/web",
		".config/app": ".config/app", // previously became "config/app"
		"..hidden":    "..hidden",    // previously became "hidden"
		"":            "",
		".":           "",
		"./":          "",
		"/":           "",
	} {
		c := base()
		c.RootDir = in
		if got := appRoot(c); got != want {
			t.Errorf("appRoot(%q) = %q, want %q", in, got, want)
		}
		// rootDirOrDot is the runner's build context; "" must become "." there.
		wantDot := want
		if wantDot == "" {
			wantDot = "."
		}
		if got := rootDirOrDot(c); got != wantDot {
			t.Errorf("rootDirOrDot(%q) = %q, want %q", in, got, wantDot)
		}
	}
}

// TestCdRoot_MatchesAppRoot keeps the script's cd and the runner's build context
// derived from the same value; they addressed the same directory by two
// different broken paths before.
func TestCdRoot_MatchesAppRoot(t *testing.T) {
	c := base()
	c.RootDir = ".config/app"
	if got := cdRoot(c); got != "cd \".config/app\"\n" {
		t.Errorf("cdRoot = %q", got)
	}
	c.RootDir = ""
	if got := cdRoot(c); got != "" {
		t.Errorf("cdRoot at repo root = %q, want empty", got)
	}
}

// Builds are named <environment>-<run number>-<first 5 of the sha>: the step that sets
// it runs before anything is built, and everything after reads it from env.
func TestWorkflow_NamesTheBuildWithAShortSHA(t *testing.T) {
	for _, strategy := range []Strategy{Compose, Docker} {
		wf := Workflow(Config{App: "web", Branch: "main", Environment: "production", Strategy: strategy, DeploymentID: testID,
			Ports: []Port{{EnvVar: "APP_PORT", Value: "3000", Container: "3000"}}})
		name := strings.Index(wf, "- name: Name this build")
		build := strings.Index(wf, "- name: Build")
		if name < 0 || build < 0 || name > build {
			t.Fatalf("%s: the naming step must come before the build", strategy)
		}
		if !strings.Contains(wf, `MOUNTABO_ARTIFACT_ID=production-${run}-${GITHUB_SHA::5}`) {
			t.Errorf("%s: build name should be <environment>-<run>-<first 5 of the sha>", strategy)
		}
		// Image tags and the artifact id use the short name; only MOUNTABO_COMMIT
		// keeps the full sha, for the release record.
		if strings.Contains(wf, ":${{ github.sha }}") || strings.Contains(wf, "-${{ github.sha }}") {
			t.Errorf("%s: build names must not carry the full sha", strategy)
		}
		if !strings.Contains(wf, "MOUNTABO_ARTIFACT_ID: ${{ env.MOUNTABO_ARTIFACT_ID }}") {
			t.Errorf("%s: the deploy step should read the build name from env", strategy)
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(wf), &doc); err != nil {
			t.Fatalf("%s: workflow is not valid YAML: %v", strategy, err)
		}
	}
}

// Compose deploys ship the compose files from the runner; nothing on the
// server clones the repository or holds a key that can read it.
func TestCompose_ShipsComposeFilesInsteadOfCloning(t *testing.T) {
	c := Config{App: "web", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production", Strategy: Compose,
		DeploymentID: testID, DeployKeyFile: "mountabo_deploy_acme_shop", Ports: []Port{{EnvVar: "APP_PORT", Value: "3000", Container: "3000"}}}
	script, wf := DeployScript(c), Workflow(c)
	for _, bad := range []string{"git clone", "git fetch", "GIT_SSH_COMMAND", "REPO_URL"} {
		if strings.Contains(script, bad) {
			t.Errorf("deploy.sh still contains %q", bad)
		}
	}
	for _, want := range []string{`tar -xzf "$COMPOSE_TAR"`, `rm -f "$HOME/.ssh/mountabo_deploy_acme_shop"`, "rmdir -p --ignore-fail-on-non-empty"} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy.sh missing %q", want)
		}
	}
	for _, want := range []string{"tar -czf mountabo.compose.tar.gz", "--null -T mountabo.compose.list", "mountabo.compose.tar.gz"} {
		if !strings.Contains(wf, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
}

// renderEnvLine runs the generated mountabo_env_line on value and returns the
// .env line it writes.
func renderEnvLine(t *testing.T, script, value string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	i := strings.Index(script, "mountabo_env_line() {")
	j := strings.Index(script[i:], "\n}\n")
	fn := script[i : i+j+3]
	cmd := exec.Command(bash, "-c", fn+`mountabo_env_line K "$V"`)
	cmd.Env = append(os.Environ(), "V="+value)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mountabo_env_line: %v\n%s", err, out)
	}
	return string(out)
}

// Compose reads .env with a dotenv parser, so values are double-quoted with
// \ " $ and line breaks escaped. These encodings were checked against a real
// docker compose: every value reached the container unchanged.
func TestComposeEnv_QuotesEveryValueSafely(t *testing.T) {
	script := DeployScript(Config{App: "web", Branch: "main", Environment: "production", Strategy: Compose, DeploymentID: testID})
	for value, want := range map[string]string{
		"plain":          `K="plain"`,
		`has "quotes"`:   `K="has \"quotes\""`,
		"it's":           `K="it's"`,
		"$HOME and ${X}": `K="\$HOME and \${X}"`,
		`back\slash`:     `K="back\\slash"`,
		"line1\nline2":   `K="line1\nline2"`,
		"":               `K=""`,
		"hash # kept":    `K="hash # kept"`,
	} {
		if got := strings.TrimSuffix(renderEnvLine(t, script, value), "\n"); got != want {
			t.Errorf("value %q: got %s, want %s", value, got, want)
		}
	}
}

// docker run --env-file takes values literally and can't hold a line break.
func TestDockerEnv_WritesRawValuesAndRefusesLineBreaks(t *testing.T) {
	script := DeployScript(Config{App: "web", Branch: "main", Environment: "production", Strategy: Docker, DeploymentID: testID})
	if got := renderEnvLine(t, script, `has "quotes" & $x`); got != "K=has \"quotes\" & $x\n" {
		t.Errorf("docker env line = %q, want the raw value", got)
	}
	bash, _ := exec.LookPath("bash")
	i := strings.Index(script, "mountabo_env_line() {")
	j := strings.Index(script[i:], "\n}\n")
	cmd := exec.Command(bash, "-c", script[i:i+j+3]+`mountabo_env_line K "$V"`)
	cmd.Env = append(os.Environ(), "V=a\nb")
	if err := cmd.Run(); err == nil {
		t.Error("a value with a line break should be refused for docker --env-file")
	}
}
