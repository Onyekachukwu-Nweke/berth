package usecase

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeStateReader returns a fixed release state, and records that it was asked.
type fakeStateReader struct {
	state ReleaseState
	err   error
	calls int
}

func (f *fakeStateReader) ReadReleaseState(context.Context, SSHTarget, string) (ReleaseState, error) {
	f.calls++
	return f.state, f.err
}

// fakeScriptRunner records the scripts it was asked to run on the server.
type fakeScriptRunner struct {
	scripts []string
	err     error
}

func (f *fakeScriptRunner) RunAsDeployUser(_ context.Context, _ SSHTarget, script string, out io.Writer) error {
	f.scripts = append(f.scripts, script)
	_, _ = io.WriteString(out, "ran\n")
	return f.err
}

// fakeRunLister reports a single run with a chosen status.
type fakeRuns struct{ status string }

func (f fakeRuns) ListWorkflowRuns(context.Context, Token, string, string, string, string, int) ([]WorkflowRun, error) {
	if f.status == "" {
		return nil, nil
	}
	return []WorkflowRun{{Status: f.status}}, nil
}

func rollbackFixture(t *testing.T, state ReleaseState) (*RollbackService, *fakeScriptRunner, *fakeStateReader) {
	t.Helper()
	deps := &fakeDeploymentStore{saved: []Deployment{{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "production", ServerID: "s1", WorkflowFile: "w.yml",
		DeploymentID: DeriveDeploymentID("acme", "shop", "production"),
		Config:       &DeployConfig{Strategy: "compose", DeployDir: "/opt/shop"},
	}}}
	servers := newMemServerStore()
	if err := servers.Save(Server{ID: "s1", Name: "box", IP: "10.0.0.1", SSHPort: 22, Status: StatusReady}); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	vault := newFakeVault()
	if err := vault.SaveSecret(privateKeyKey("s1"), "KEY"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	reader := &fakeStateReader{state: state}
	runner := &fakeScriptRunner{}
	svc := NewRollbackService(deps, servers, vault, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRuns{}, reader, runner, NewDeployLocker(), nil)
	return svc, runner, reader
}

func TestRollback_DefaultsToThePreviousRelease(t *testing.T) {
	svc, runner, _ := rollbackFixture(t, ReleaseState{Active: "art-2", Previous: "art-1", Lifecycle: "active"})

	var out strings.Builder
	if err := svc.Rollback(context.Background(), RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}, &out); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Fatalf("want one script run, got %d", len(runner.scripts))
	}
	s := runner.scripts[0]
	if !strings.Contains(s, "MOUNTABO_ARTIFACT='art-1'") {
		t.Errorf("rollback should target the previous release:\n%s", s)
	}
	if !strings.Contains(s, "MOUNTABO_MODE=rollback") || !strings.Contains(s, "MOUNTABO_KIND='rollback'") {
		t.Errorf("script should set the rollback markers:\n%s", s)
	}
	// It must exec the runner bundled with the TARGET release, not the currently
	// committed script: an older bundle handed to a newer script is exactly the
	// compatibility break the bundled copy exists to avoid.
	if !strings.Contains(s, `script="$HOME/.mountabo/releases/$deployment/$artifact/runner.sh"`) {
		t.Errorf("rollback should exec the bundled runner:\n%s", s)
	}
}

// TestRollback_RefusesAnArtifactThatIsNoLongerRetained is the honesty rule:
// history lists every release, but retention keeps only the active one and its
// predecessor, so anything else cannot be started however much the UI shows.
func TestRollback_RefusesAnArtifactThatIsNoLongerRetained(t *testing.T) {
	svc, runner, _ := rollbackFixture(t, ReleaseState{Active: "art-3", Previous: "art-2"})

	err := svc.Rollback(context.Background(), RollbackInput{
		Owner: "acme", Repo: "shop", Environment: "production", ArtifactID: "art-1",
	}, io.Discard)
	if !errors.Is(err, ErrArtifactNotRetained) {
		t.Errorf("want ErrArtifactNotRetained, got %v", err)
	}
	if len(runner.scripts) != 0 {
		t.Errorf("nothing should have run on the server")
	}
}

func TestRollback_RefusesWhenThereIsNoEarlierRelease(t *testing.T) {
	svc, runner, _ := rollbackFixture(t, ReleaseState{Active: "art-1"})
	err := svc.Rollback(context.Background(), RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}, io.Discard)
	if !errors.Is(err, ErrNoPreviousRelease) {
		t.Errorf("want ErrNoPreviousRelease, got %v", err)
	}
	if len(runner.scripts) != 0 {
		t.Errorf("nothing should have run on the server")
	}
}

// TestRollback_RefusesWhileBlocked covers the migration gate reaching back into
// rollback: if nobody knows whether a migration applied, changing what is running
// is exactly the thing that must not happen.
func TestRollback_RefusesWhileBlocked(t *testing.T) {
	svc, runner, _ := rollbackFixture(t, ReleaseState{Active: "art-2", Previous: "art-1", Lifecycle: "blocked"})
	err := svc.Rollback(context.Background(), RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}, io.Discard)
	if !errors.Is(err, ErrDeploymentBlocked) {
		t.Errorf("want ErrDeploymentBlocked, got %v", err)
	}
	if len(runner.scripts) != 0 {
		t.Errorf("nothing should have run while blocked")
	}
}

// TestRedeploy_TargetsTheActiveArtifact pins that a redeploy cannot start
// something other than what is supposed to be running — it re-points at the
// recorded active artifact rather than resolving anything fresh.
func TestRedeploy_TargetsTheActiveArtifact(t *testing.T) {
	svc, runner, _ := rollbackFixture(t, ReleaseState{Active: "art-2", Previous: "art-1"})

	if err := svc.Redeploy(context.Background(), "acme", "shop", "production", io.Discard); err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	s := runner.scripts[0]
	if !strings.Contains(s, "MOUNTABO_ARTIFACT='art-2'") {
		t.Errorf("redeploy should target the active release:\n%s", s)
	}
	if !strings.Contains(s, "MOUNTABO_KIND='redeploy'") {
		t.Errorf("redeploy should record its kind:\n%s", s)
	}
}

// TestRollback_HoldsAndReleasesTheLock keeps a rollback from racing a deploy
// mountabo starts, and — just as important — makes sure a failure still frees the
// lock rather than wedging the deployment.
func TestRollback_HoldsAndReleasesTheLock(t *testing.T) {
	svc, _, _ := rollbackFixture(t, ReleaseState{Active: "art-2", Previous: "art-1"})
	key := lockKey("acme", "shop", "production")

	svc.locker.TryAcquire(key)
	err := svc.Rollback(context.Background(), RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}, io.Discard)
	if !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("want ErrDeployInProgress while the lock is held, got %v", err)
	}
	svc.locker.Release(key)

	if err := svc.Rollback(context.Background(), RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}, io.Discard); err != nil {
		t.Fatalf("Rollback after release: %v", err)
	}
	// And the lock is free again afterwards.
	if !svc.locker.TryAcquire(key) {
		t.Errorf("lock was not released after a successful rollback")
	}
}

// TestRollback_FailsFastOnAnInFlightRun keeps the common case from waiting on the
// server lock for minutes. It is advisory: the lock on the box is the guarantee,
// which is why --force can skip it.
func TestRollback_FailsFastOnAnInFlightRun(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{{
		ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main",
		Environment: "production", ServerID: "s1", WorkflowFile: "w.yml",
		Config: &DeployConfig{Strategy: "compose", DeployDir: "/opt/shop"},
	}}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", Name: "box", IP: "10.0.0.1", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")
	runner := &fakeScriptRunner{}
	svc := NewRollbackService(deps, servers, vault, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRuns{status: "in_progress"}, &fakeStateReader{state: ReleaseState{Active: "art-2", Previous: "art-1"}},
		runner, NewDeployLocker(), nil)

	in := RollbackInput{Owner: "acme", Repo: "shop", Environment: "production"}
	if err := svc.Rollback(context.Background(), in, io.Discard); !errors.Is(err, ErrRunInProgress) {
		t.Errorf("want ErrRunInProgress, got %v", err)
	}
	if len(runner.scripts) != 0 {
		t.Errorf("nothing should have run while a deploy is in flight")
	}

	// Force skips the advisory check; the server's own lock still serializes.
	in.Force = true
	if err := svc.Rollback(context.Background(), in, io.Discard); err != nil {
		t.Errorf("force should proceed past the advisory check: %v", err)
	}
	if len(runner.scripts) != 1 {
		t.Errorf("forced rollback should have run")
	}
}

func TestParseReleaseState_TreatsAMissingDocumentAsEmpty(t *testing.T) {
	// A deployment that has not run since release tracking shipped has no
	// document. That must not stop the UI showing the deployment.
	st, err := ParseReleaseState("   ")
	if err != nil || st.Active != "" {
		t.Errorf("empty document should parse to an empty state: %+v err=%v", st, err)
	}
	if _, err := ParseReleaseState("{not json"); err == nil {
		t.Errorf("a corrupt document should be reported, not silently ignored")
	}
}
