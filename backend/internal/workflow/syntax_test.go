package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedScriptsParse runs `bash -n` over every script this package can
// emit. It is the cheapest guard available against the riskiest artifact here:
// deploy.sh is assembled by string concatenation, is only ever executed on a
// remote server via GitHub Actions, and a syntax error in it does not surface
// until a real deploy fails somewhere an operator cannot easily read. Nothing
// else in the suite checks that the generated shell even parses.
//
// Every configuration shape that changes the emitted text belongs in the matrix,
// so a fragment added for one case cannot silently break another.
func TestGeneratedScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping generated-script syntax check")
	}

	withEnv := func(c Config, vars ...EnvVar) Config { c.EnvVars = vars; return c }

	cases := map[string]Config{}
	for _, strategy := range []Strategy{Compose, Docker} {
		name := string(strategy)

		full := base()
		full.Strategy = strategy
		full.Environment = "production"
		full.DeployKeyFile = "mountabo_deploy_acme_shop"
		cases[name+"/full"] = full

		// No env vars: envWriteBlock takes its ": > .env" branch instead of the
		// heredoc, and the workflow's forward list collapses to DEPLOY_DIR.
		cases[name+"/no-env-vars"] = withEnv(full)

		// No ports: the compose export block and the docker -p flags disappear.
		noPorts := full
		noPorts.Ports = nil
		cases[name+"/no-ports"] = noPorts

		// Repo root: cdRoot emits nothing, so the surrounding lines must still join.
		root := full
		root.RootDir = ""
		cases[name+"/repo-root"] = root

		// No deploy key: gitAuth emits nothing.
		noKey := full
		noKey.DeployKeyFile = ""
		cases[name+"/no-deploy-key"] = noKey

		// A branch containing "/" — legal in git, illegal in a docker tag, and the
		// reason Slug exists. It must not produce unparseable shell either.
		slashy := full
		slashy.Branch = "feature/big-thing"
		cases[name+"/slash-branch"] = slashy

		// Values and names that are shell-significant if ever interpolated
		// unquoted. Only the KEY reaches the script, but a key is still emitted
		// into the workflow's env block and MOUNTABO_ENV_KEYS list.
		hostile := withEnv(full,
			EnvVar{Key: "DATABASE_URL", Value: `p@ss w'ord$(id)`},
			EnvVar{Key: "A_B_C", Value: "#comment ${x} `id`"},
		)
		cases[name+"/hostile-values"] = hostile
	}

	dir := t.TempDir()
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			script := DeployScript(cfg)
			path := filepath.Join(dir, strings.ReplaceAll(name, "/", "_")+".sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write script: %v", err)
			}
			out, err := exec.Command(bash, "-n", path).CombinedOutput()
			if err != nil {
				t.Fatalf("generated script is not valid bash: %v\n%s\n--- script ---\n%s", err, out, script)
			}
		})
	}
}

// TestGeneratedScriptsAreBash pins the interpreter the syntax check assumes.
// envWriteBlock uses ${!k} indirect expansion, which is a bash feature — running
// the script under a POSIX sh would silently fail to inject any variable.
func TestGeneratedScriptsAreBash(t *testing.T) {
	for _, strategy := range []Strategy{Compose, Docker} {
		c := base()
		c.Strategy = strategy
		s := DeployScript(c)
		if !strings.HasPrefix(s, "#!/usr/bin/env bash") {
			t.Errorf("%s script must declare bash (it relies on ${!var} indirect expansion):\n%s", strategy, strings.SplitN(s, "\n", 2)[0])
		}
	}
}
