package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runEnvWrite executes the generated .env-writing block for real, with the given
// variables exported the way appleboy/ssh-action forwards them, and returns the
// resulting .env plus the block's output.
//
// This block is executed rather than string-matched because the bug it fixes is
// a shell-semantics bug: the old code tested `[ -z "$v" ]`, which cannot tell an
// unset variable from one set to the empty string. Only running it proves the
// distinction now holds.
func runEnvWrite(t *testing.T, c Config, includePorts bool, keys string, exports ...string) (envFile string, output string, code int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	script := "#!/usr/bin/env bash\nset -euo pipefail\ncd " + dir + "\n" +
		"export MOUNTABO_ENV_KEYS=" + shellQuoteForTest(keys) + "\n" +
		envWriteBlock(c, includePorts)
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bash, path)
	cmd.Env = append(os.Environ(), exports...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v\n%s", err, out)
		}
	}
	b, rerr := os.ReadFile(filepath.Join(dir, ".env"))
	if rerr != nil && code == 0 {
		t.Fatalf("no .env written: %v\n%s", rerr, out)
	}
	return string(b), string(out), code
}

func shellQuoteForTest(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestEnvWrite_KeepsDeliberatelyEmptyValues(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "DATABASE_URL"}, {Key: "FEATURE_FLAG"}}

	// FEATURE_FLAG is exported as the empty string: the operator configured it and
	// chose "". It must reach the container as an empty value, not vanish — for
	// most runtimes an unset variable and an empty one behave differently.
	env, out, code := runEnvWrite(t, c, false, "DATABASE_URL FEATURE_FLAG",
		"DATABASE_URL=postgres://x", "FEATURE_FLAG=")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(env, "FEATURE_FLAG=\n") && !strings.HasSuffix(env, "FEATURE_FLAG=") {
		t.Errorf("empty value was dropped from .env:\n%q", env)
	}
	if !strings.Contains(env, "DATABASE_URL=postgres://x") {
		t.Errorf(".env missing the normal value:\n%q", env)
	}
	if strings.Contains(out, "not set") {
		t.Errorf("an empty-but-set variable was reported as unset:\n%s", out)
	}
	if !strings.Contains(out, "wrote 2 environment variable(s)") {
		t.Errorf("count should include the empty value:\n%s", out)
	}
}

func TestEnvWrite_ReportsAVariableThatNeverArrived(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "DATABASE_URL"}, {Key: "MISSING_ONE"}}

	// MISSING_ONE is configured in mountabo but never reached the shell — not set
	// on the GitHub environment, or not forwarded. That is worth reporting.
	env, out, code := runEnvWrite(t, c, false, "DATABASE_URL MISSING_ONE", "DATABASE_URL=x")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(env, "MISSING_ONE") {
		t.Errorf("an unset variable should not be written:\n%q", env)
	}
	if !strings.Contains(out, "MISSING_ONE is not set on this GitHub environment") {
		t.Errorf("missing variable was not reported by name:\n%s", out)
	}
	if !strings.Contains(out, "wrote 1 environment variable(s)") {
		t.Errorf("count wrong:\n%s", out)
	}
}

func TestEnvWrite_FailsWhenNothingArrivedAtAll(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "DATABASE_URL"}}
	// Variables were configured but none reached the box: silently deploying an
	// app with no configuration is worse than failing the run.
	_, out, code := runEnvWrite(t, c, false, "DATABASE_URL")
	if code == 0 {
		t.Errorf("deploy continued with an empty .env:\n%s", out)
	}
}

func TestEnvWrite_PreservesValuesVerbatim(t *testing.T) {
	c := base()
	c.EnvVars = []EnvVar{{Key: "TRICKY"}}
	// The value is written by printf with no interpretation, so shell-significant
	// characters must land byte-for-byte rather than being expanded or truncated.
	const value = `p@ss w'ord$(id)#not-a-comment ${x}`
	env, out, code := runEnvWrite(t, c, false, "TRICKY", "TRICKY="+value)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(env, "TRICKY="+value) {
		t.Errorf("value was not written verbatim:\n got %q\nwant %q", env, "TRICKY="+value)
	}
}
