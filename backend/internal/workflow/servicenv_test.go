package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runServiceEnv executes the generated per-service env-file block inside a real
// git working tree, twice, the way two consecutive deploys would.
//
// A real repo is required because the block's correctness hinges on git: it asks
// "git ls-files" whether a file is committed, restores committed files with "git
// checkout --", and the whole bug exists because "git reset --hard" (what
// clonePull does between deploys) leaves untracked files untouched.
func runServiceEnv(t *testing.T, compose string, committed map[string]string) func(envBody string) map[string]string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()

	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o600); err != nil {
		t.Fatalf("compose: %v", err)
	}
	for path, body := range committed {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	run("git", "add", "-A")
	run("git", "commit", "-qm", "seed")

	return func(envBody string) map[string]string {
		t.Helper()
		// A deploy resets the tree, then rewrites the root .env, then runs this.
		run("git", "reset", "--hard", "-q")
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envBody), 0o600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		script := "#!/usr/bin/env bash\nset -euo pipefail\ncd " + dir + "\n" + populateEnvFiles
		path := filepath.Join(dir, "run.sh")
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatalf("write script: %v", err)
		}
		cmd := exec.Command(bash, path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("service-env block failed: %v\n%s", err, out)
		}
		got := map[string]string{}
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".env") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr == nil {
				rel, _ := filepath.Rel(dir, p)
				got[rel] = string(b)
			}
			return nil
		})
		return got
	}
}

// TestServiceEnvFiles_RefreshOnEveryDeploy is the regression test for the primary
// env-injection defect: a service's env_file was written only when missing, so
// after the first deploy it kept those values forever and no later secret change
// ever reached that service.
func TestServiceEnvFiles_RefreshOnEveryDeploy(t *testing.T) {
	deploy := runServiceEnv(t, `services:
  api:
    image: x
    env_file:
      - ./api/.env
`, nil)

	first := deploy("DATABASE_URL=old\n")
	if got := first["api/.env"]; !strings.Contains(got, "DATABASE_URL=old") {
		t.Fatalf("first deploy did not populate api/.env: %q", got)
	}

	// The operator changes the secret and redeploys. Before the fix, api/.env was
	// untracked and already present, so it was skipped and kept "old" — while the
	// root .env correctly showed "new", which is what made this so hard to spot.
	second := deploy("DATABASE_URL=new\n")
	if got := second["api/.env"]; !strings.Contains(got, "DATABASE_URL=new") {
		t.Errorf("api/.env is stale after a secret change: %q", got)
	}
	if got := second["api/.env"]; strings.Contains(got, "DATABASE_URL=old") {
		t.Errorf("api/.env still carries the previous value: %q", got)
	}
}

// TestServiceEnvFiles_CommittedDefaultsSurviveButOperatorWins covers the case
// that makes a blind overwrite wrong: an env file the repo actually commits.
func TestServiceEnvFiles_CommittedDefaultsSurviveButOperatorWins(t *testing.T) {
	deploy := runServiceEnv(t, `services:
  api:
    image: x
    env_file:
      - ./api/.env
`, map[string]string{
		// No trailing newline: appending naively would fuse LOG_LEVEL's line with
		// the first appended entry into one malformed line.
		"api/.env": "LOG_LEVEL=debug\nDATABASE_URL=committed-default",
	})

	got := deploy("DATABASE_URL=from-mountabo\n")["api/.env"]
	if !strings.Contains(got, "LOG_LEVEL=debug") {
		t.Errorf("committed default was lost: %q", got)
	}
	// Later assignments win for both dotenv and docker --env-file, so the
	// operator's value must come after the committed one.
	iCommitted := strings.Index(got, "DATABASE_URL=committed-default")
	iOperator := strings.Index(got, "DATABASE_URL=from-mountabo")
	if iOperator < 0 {
		t.Fatalf("operator value missing: %q", got)
	}
	if iCommitted >= 0 && iOperator < iCommitted {
		t.Errorf("operator value must come last so it wins:\n%q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Count(line, "=") > 1 && strings.Contains(line, "committed-default") {
			t.Errorf("lines fused together (missing trailing newline handling): %q", line)
		}
	}

	// Redeploying must not accumulate: the committed file is restored from git
	// first, so the operator's values are appended once, not once per deploy.
	again := deploy("DATABASE_URL=from-mountabo\n")["api/.env"]
	if n := strings.Count(again, "DATABASE_URL=from-mountabo"); n != 1 {
		t.Errorf("operator value appended %d times across deploys:\n%q", n, again)
	}
}

// TestServiceEnvFiles_SkipsTheProjectEnvItself covers "env_file: .env", where the
// source and destination are the same file: copying it onto itself fails and
// appending would double every line on every deploy.
func TestServiceEnvFiles_SkipsTheProjectEnvItself(t *testing.T) {
	deploy := runServiceEnv(t, `services:
  api:
    image: x
    env_file:
      - .env
`, nil)

	got := deploy("DATABASE_URL=value\n")[".env"]
	if n := strings.Count(got, "DATABASE_URL=value"); n != 1 {
		t.Errorf("project .env was duplicated onto itself %d times:\n%q", n, got)
	}
	again := deploy("DATABASE_URL=value\n")[".env"]
	if n := strings.Count(again, "DATABASE_URL=value"); n != 1 {
		t.Errorf("project .env grew across deploys (%d entries):\n%q", n, again)
	}
}
