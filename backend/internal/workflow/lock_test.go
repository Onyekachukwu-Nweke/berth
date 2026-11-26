package workflow

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPreamble executes the generated lock preamble for real against a scratch
// state directory and returns its combined output and exit code.
//
// The preamble is executed rather than string-matched because what matters is
// its behaviour — whether a run actually stops — and that is decided by shell
// semantics (jq defaults, numeric comparison under `set -e`, the flock exit
// path) that reading the text cannot confirm. The script body is replaced with a
// marker so the test observes exactly one thing: did control reach the deploy.
func runPreamble(t *testing.T, c Config, state string, env ...string) (string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, tool := range []string{"jq", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available; the preamble requires it", tool)
		}
	}

	// The header normally defines these; the preamble is exercised on its own, so
	// they are supplied here. `set -euo pipefail` matches the generated script, so
	// an unbound reference in the preamble fails this harness the way it would
	// fail a real deploy.
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		fmt.Sprintf("DEPLOYMENT_ID=%q\n", c.deploymentID()) +
		"DEPLOY_ROOT=/opt/shop\nDEPLOY_DIR=\"$DEPLOY_ROOT/$DEPLOYMENT_ID\"\nLEGACY_DEPLOY_DIR=\"$DEPLOY_ROOT/main\"\n" +
		lockPreamble(c) +
		"\necho REACHED_DEPLOY\n"

	path := filepath.Join(t.TempDir(), "preamble.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bash, path)
	cmd.Env = append(os.Environ(), "MOUNTABO_STATE="+state, "HOME="+state)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v\n%s", err, out)
		}
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

func writeState(t *testing.T, state, id, doc string) {
	t.Helper()
	dir := filepath.Join(state, "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(doc), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func lockCfg() Config {
	c := base()
	c.Environment = "production"
	c.DeploymentID = testID
	c.Incarnation = 2
	return c
}

func TestLockPreamble_ProceedsWithNoStateYet(t *testing.T) {
	// A deployment that has never run has no state document. That must not be
	// mistaken for a tombstone.
	out, code := runPreamble(t, lockCfg(), t.TempDir())
	if code != 0 || !strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("first-ever deploy was blocked (exit %d):\n%s", code, out)
	}
}

func TestLockPreamble_StopsAfterDeletion(t *testing.T) {
	// The acceptance property: a run queued before a deployment was deleted must
	// not recreate the app that was just torn down. The tombstone outlives purge
	// precisely so this check has something to find.
	for _, lifecycle := range []string{"deleting", "deleted"} {
		state := t.TempDir()
		writeState(t, state, testID, `{"lifecycle":"`+lifecycle+`","incarnation":2}`)
		out, code := runPreamble(t, lockCfg(), state)
		if strings.Contains(out, "REACHED_DEPLOY") {
			t.Errorf("lifecycle %q still deployed:\n%s", lifecycle, out)
		}
		// Exit 0: the run did the right thing. A red Actions run would suggest a
		// fault the operator needs to fix, and there is nothing to fix.
		if code != 0 {
			t.Errorf("lifecycle %q exited %d, want 0 with an explanation", lifecycle, code)
		}
	}
}

func TestLockPreamble_StopsWhileBlockedOnAMigration(t *testing.T) {
	// An unresolved migration outcome means nobody knows whether the schema
	// changed. Deploying into that is the one thing that must not happen, and it
	// is a real fault, so it fails loudly rather than exiting 0.
	state := t.TempDir()
	writeState(t, state, testID, `{"lifecycle":"blocked","incarnation":2}`)
	out, code := runPreamble(t, lockCfg(), state)
	if strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("deployed while blocked:\n%s", out)
	}
	if code == 0 {
		t.Errorf("blocked deployment exited 0; it should fail loudly:\n%s", out)
	}
}

func TestLockPreamble_RejectsASupersededIncarnation(t *testing.T) {
	// Recreating a deleted deployment bumps the incarnation. A run generated
	// under the old one is stale by definition and must not deploy into its
	// successor's resources.
	state := t.TempDir()
	writeState(t, state, testID, `{"lifecycle":"active","incarnation":5}`)
	c := lockCfg()
	c.Incarnation = 2
	out, code := runPreamble(t, c, state)
	if strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("superseded run still deployed:\n%s", out)
	}
	if code != 0 {
		t.Errorf("superseded run exited %d, want 0 with an explanation", code)
	}

	// The current incarnation still deploys.
	c.Incarnation = 5
	if out, code := runPreamble(t, c, state); code != 0 || !strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("current incarnation was blocked (exit %d):\n%s", code, out)
	}
}

func TestLockPreamble_ToleratesAnUnreadableStateDocument(t *testing.T) {
	// A truncated or corrupt state file must not wedge deploys permanently; jq
	// failing has to fall back to "active" rather than aborting under `set -e`.
	state := t.TempDir()
	writeState(t, state, testID, `{"lifecycle": `)
	out, code := runPreamble(t, lockCfg(), state)
	if code != 0 || !strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("a corrupt state document blocked the deploy (exit %d):\n%s", code, out)
	}
}

func TestLockPreamble_SerializesConcurrentOperations(t *testing.T) {
	// The whole point of the host lock: an SSH-direct operation and an Actions run
	// never see each other's application-level locks, so they must queue here.
	// A second entrant with a zero wait must be refused while the first holds it.
	state := t.TempDir()
	c := lockCfg()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available")
	}

	// Hold the lock out-of-band, exactly as a concurrent operation would.
	lockDir := filepath.Join(state, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockFile := filepath.Join(lockDir, c.deploymentID()+".lock")
	holder := exec.Command(bash, "-c", "exec 9>\""+lockFile+"\"; flock 9; sleep 5")
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	defer func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() }()

	// Give the holder a moment to actually acquire it.
	waited := exec.Command(bash, "-c", "for i in $(seq 1 50); do if ! flock -n -w 0 \""+lockFile+"\" true 2>/dev/null; then exit 0; fi; sleep 0.1; done; exit 1")
	if err := waited.Run(); err != nil {
		t.Skip("could not observe the lock being held; environment does not support this check")
	}

	out, code := runPreamble(t, c, state, "MOUNTABO_LOCK_WAIT=0")
	if strings.Contains(out, "REACHED_DEPLOY") {
		t.Errorf("second operation ran while the lock was held:\n%s", out)
	}
	if code == 0 {
		t.Errorf("contended lock exited 0; it should fail loudly:\n%s", out)
	}
}
