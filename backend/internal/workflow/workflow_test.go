package workflow

import (
	"strings"
	"testing"
)

func base() Config {
	return Config{
		App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		RootDir: "./apps/web", DeployDir: "/opt/shop",
		Ports:   []Port{{EnvVar: "FRONTEND_PORT", Value: "3000", Container: "3000"}},
		EnvVars: []EnvVar{{Key: "DATABASE_URL", Value: "x"}},
	}
}

func TestDeployScript_Compose(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)
	for _, want := range []string{"docker compose $COMPOSE_ARGS build", "docker compose $COMPOSE_ARGS up -d", "export FRONTEND_PORT=3000", `cd "apps/web"`} {
		if !strings.Contains(s, want) {
			t.Errorf("compose script missing %q", want)
		}
	}
	if strings.Contains(s, "docker run") {
		t.Error("compose script should not use docker run")
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
		"docker compose $COMPOSE_ARGS build",
		"docker compose $COMPOSE_ARGS up -d",
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
	for _, want := range []string{"docker build -t", "docker run -d", "-p 3000:3000", `--restart unless-stopped`} {
		if !strings.Contains(s, want) {
			t.Errorf("docker script missing %q", want)
		}
	}
	if strings.Contains(s, "docker compose") {
		t.Error("docker script should not use docker compose")
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
	for _, want := range []string{"wrote $written environment variable(s) to .env", "is empty", "exit 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("deploy.sh missing verification %q:\n%s", want, s)
		}
	}
	// Ports remain baked (they are not secrets).
	if !strings.Contains(s, "FRONTEND_PORT=${FRONTEND_PORT}") {
		t.Errorf("compose .env should still seed baked port vars:\n%s", s)
	}
}

func TestWorkflow_PinsEnvironmentAndSecrets(t *testing.T) {
	c := base()
	c.Environment = "production"
	w := Workflow(c)
	for _, want := range []string{"environment: production", "DATABASE_URL: ${{ secrets.DATABASE_URL }}", "envs: DATABASE_URL,DEPLOY_DIR", "deploy.sh main"} {
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
