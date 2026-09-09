package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runComposeOverride executes the generated service-discovery block with a stub
// "docker" on PATH, so the block's behaviour can be observed without a real
// docker daemon. The stub's script decides what "docker compose config" does.
//
// This is executed rather than string-matched because the defect was a silent
// one: discovery swallowed stderr and, when it produced nothing, the deploy went
// on to ship an application with no configuration at all while reporting success.
// Only running it proves the run now stops.
func runComposeOverride(t *testing.T, c Config, dockerStub string, composeFiles map[string]string) (output string, code int, wrote string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/usr/bin/env bash\n"+dockerStub), 0o755); err != nil {
		t.Fatalf("stub: %v", err)
	}
	for name, body := range composeFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("compose: %v", err)
		}
	}

	// artifactIdentityBlock normally defines this earlier in the script; the
	// override is exercised on its own, so it is supplied here. "set -euo
	// pipefail" matches the generated script, so an unbound reference fails this
	// harness the way it would fail a real deploy.
	script := "#!/usr/bin/env bash\nset -euo pipefail\ncd " + dir + "\n" +
		"ARTIFACT_ID=test-artifact\n" + composeEnvOverride(c) + "\necho REACHED_BUILD\n"
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bash, path)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v\n%s", err, out)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "mountabo.env.override.yml"))
	return string(out), code, string(b)
}

const stubComposeOK = `
if [ "$1" = compose ]; then
  for a in "$@"; do
    if [ "$a" = "--format" ]; then
      echo '{"services":{"web":{},"api":{},"db":{}}}'
      exit 0
    fi
  done
fi
exit 0
`

// A compose file that references an undefined required variable is the realistic
// way discovery fails: compose exits non-zero and explains why on stderr.
const stubComposeFails = `
if [ "$1" = compose ]; then
  echo 'error: required variable POSTGRES_PASSWORD is missing a value' >&2
  exit 1
fi
exit 0
`

func TestComposeOverride_InjectsIntoEveryService(t *testing.T) {
	c := base()
	c.Strategy = Compose
	out, code, wrote := runComposeOverride(t, c, stubComposeOK, map[string]string{"docker-compose.yml": "services:\n  web:\n"})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, svc := range []string{"web", "api", "db"} {
		if !strings.Contains(wrote, "  "+svc+":") {
			t.Errorf("service %q missing from the override:\n%s", svc, wrote)
		}
	}
	if strings.Count(wrote, "- .env") != 3 {
		t.Errorf("every service should get env_file: .env:\n%s", wrote)
	}
}

// TestComposeOverride_FailsLoudlyWhenServicesCannotBeRead is the regression test
// for the silent no-op: discovery failing used to leave the override unwritten
// and let the deploy continue, so not one variable reached any container and the
// run still reported success.
func TestComposeOverride_FailsLoudlyWhenServicesCannotBeRead(t *testing.T) {
	c := base()
	c.Strategy = Compose
	out, code, wrote := runComposeOverride(t, c, stubComposeFails, map[string]string{"docker-compose.yml": "services:\n  web:\n"})
	if strings.Contains(out, "REACHED_BUILD") {
		t.Errorf("deploy continued with no configuration injected:\n%s", out)
	}
	if code == 0 {
		t.Errorf("exit 0 despite injecting nothing:\n%s", out)
	}
	if wrote != "" {
		t.Errorf("an override was written despite discovery failing:\n%s", wrote)
	}
	// The operator has to be told what compose actually said, or the failure is
	// just as opaque as the silent version was.
	if !strings.Contains(out, "POSTGRES_PASSWORD") {
		t.Errorf("compose's own error was not surfaced:\n%s", out)
	}
}

// TestComposeOverride_FindsAConfiguredComposeFile covers the second silent
// failure: only four hardcoded names were matched, so a project using
// docker-compose.prod.yml was never found.
func TestComposeOverride_FindsAConfiguredComposeFile(t *testing.T) {
	c := base()
	c.Strategy = Compose
	c.ComposeFile = "docker-compose.prod.yml"

	out, code, wrote := runComposeOverride(t, c, stubComposeOK, map[string]string{"docker-compose.prod.yml": "services:\n  web:\n"})
	if code != 0 {
		t.Fatalf("configured compose file was not found (exit %d):\n%s", code, out)
	}
	if !strings.Contains(wrote, "web:") {
		t.Errorf("no override written:\n%s", wrote)
	}

	// And with no compose file at all, the run stops with a message naming what
	// it looked for, rather than proceeding.
	out, code, _ = runComposeOverride(t, c, stubComposeOK, nil)
	if code == 0 || strings.Contains(out, "REACHED_BUILD") {
		t.Errorf("missing compose file did not stop the deploy:\n%s", out)
	}
	if !strings.Contains(out, "docker-compose.prod.yml") {
		t.Errorf("error should name the files it looked for:\n%s", out)
	}
}

// TestComposeOverride_FallsBackForOlderComposePlugins keeps compatibility with
// plugins that predate `config --format json`.
func TestComposeOverride_FallsBackForOlderComposePlugins(t *testing.T) {
	const stubNoFormat = `
if [ "$1" = compose ]; then
  for a in "$@"; do
    if [ "$a" = "--format" ]; then
      echo 'unknown flag: --format' >&2
      exit 1
    fi
  done
  echo web
  echo api
  exit 0
fi
exit 0
`
	c := base()
	c.Strategy = Compose
	out, code, wrote := runComposeOverride(t, c, stubNoFormat, map[string]string{"docker-compose.yml": "services:\n  web:\n"})
	if code != 0 {
		t.Fatalf("fallback to --services failed (exit %d):\n%s", code, out)
	}
	for _, svc := range []string{"web", "api"} {
		if !strings.Contains(wrote, "  "+svc+":") {
			t.Errorf("service %q missing via the fallback:\n%s", svc, wrote)
		}
	}
}
