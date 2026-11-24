package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeRunLister struct {
	runs map[string][]WorkflowRun // keyed "owner/repo"
}

func (f fakeRunLister) ListWorkflowRuns(_ context.Context, _ Token, owner, repo, _, _ string, _ int) ([]WorkflowRun, error) {
	return f.runs[owner+"/"+repo], nil
}

type fakeReleaseReader struct {
	releases []Release
	total    int
}

func (f fakeReleaseReader) Releases(_, _, _ string, _ int) ([]Release, int, error) {
	return f.releases, f.total, nil
}

// fakeContainerTeardown records the purge specs it was asked to apply, so a test
// can assert teardown ran against the right deployment — and only it — with the
// options the operator actually chose.
type fakeContainerTeardown struct {
	removed []string
	specs   []PurgeSpec
	err     error
}

func (f *fakeContainerTeardown) PurgeApp(_ context.Context, _ SSHTarget, spec PurgeSpec, out io.Writer) error {
	f.removed = append(f.removed, spec.DeploymentID)
	f.specs = append(f.specs, spec)
	_, _ = io.WriteString(out, "removed: containers\n")
	return f.err
}

// fakeRepoFileDeleter records the repo paths it was asked to delete.
type fakeRepoFileDeleter struct {
	deleted []string
	err     error
}

func (f *fakeRepoFileDeleter) DeleteFile(_ context.Context, _ Token, owner, repo, _, path, _ string) error {
	f.deleted = append(f.deleted, owner+"/"+repo+":"+path)
	return f.err
}

// fakeRepoEnvDeleter records every environment it was asked to delete, so tests
// can assert that teardown undoes the deployment environment.
type fakeRepoEnvDeleter struct {
	deleted []string
	err     error
}

func (f *fakeRepoEnvDeleter) DeleteEnvironment(_ context.Context, _ Token, owner, repo, name string) error {
	f.deleted = append(f.deleted, owner+"/"+repo+":"+name)
	return f.err
}

// fakeRepoKeyDeleter records every (repo, title prefix) it was asked to clear.
type fakeRepoKeyDeleter struct {
	cleared []string
	err     error
}

func (f *fakeRepoKeyDeleter) DeleteDeployKey(_ context.Context, _ Token, owner, repo, titlePrefix string) error {
	f.cleared = append(f.cleared, owner+"/"+repo+":"+titlePrefix)
	return f.err
}

func TestMonitorHistory_EnrichesWithRuns(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{App: "shop", Owner: "acme", Repo: "shop", Branch: "main", ServerID: "s1", WorkflowFile: "mountabo-deploy-main.yml"},
	}}
	now := time.Now()
	runs := fakeRunLister{runs: map[string][]WorkflowRun{
		"acme/shop": {
			{SHA: "abcdef1234", Title: "latest", Status: "completed", Conclusion: "success", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-1 * time.Minute)},
			{SHA: "0000000", Title: "older", Status: "completed", Conclusion: "failure", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		},
	}}
	// Releases are stored as pending; the read path overlays live status/run URL
	// from the run that built each (matched by commit SHA). The first matches the
	// successful run; the second has no matching run and keeps its stored status.
	releases := fakeReleaseReader{total: 3, releases: []Release{
		{At: now.Add(-time.Minute), Environment: "main", CommitSHA: "abcdef1234", Status: "pending", Actor: "octocat"},
		{At: now.Add(-time.Hour), Environment: "main", CommitSHA: "deadbeef", Status: "pending"},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", IP: "203.0.113.7"})
	svc := NewMonitorService(deps, releases, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, runs, servers, newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)

	got, err := svc.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 deployment, got %d", len(got))
	}
	d := got[0]
	// Latest run is a success, so the deployment reads as live.
	if d.Repo != "acme/shop" || d.Status != "live" {
		t.Errorf("unexpected status: %+v", d)
	}
	// With no domain, the live URL is the server's IP over HTTP.
	if d.LiveURL != "http://203.0.113.7" {
		t.Errorf("liveURL = %q, want http://203.0.113.7", d.LiveURL)
	}
	if d.WorkflowURL != "https://github.com/acme/shop/actions/workflows/mountabo-deploy-main.yml" {
		t.Errorf("workflowURL = %q", d.WorkflowURL)
	}
	if d.Runs[0].CommitURL != "https://github.com/acme/shop/commit/abcdef1234" {
		t.Errorf("run[0] commitURL = %q", d.Runs[0].CommitURL)
	}
	// Tracking: total deploy count + recent timeline surfaced.
	if d.Deploys != 3 {
		t.Errorf("deploys = %d, want 3", d.Deploys)
	}
	if len(d.Timeline) != 2 || d.Timeline[0].Environment != "main" {
		t.Errorf("timeline wrong: %+v", d.Timeline)
	}
	// First release joins the successful run by SHA: live status + actor + short SHA.
	if d.Timeline[0].Status != "success" || d.Timeline[0].SHA != "abcdef1" || d.Timeline[0].Actor != "octocat" {
		t.Errorf("release[0] overlay wrong: %+v", d.Timeline[0])
	}
	// Second release has no matching run, so it keeps its stored pending status.
	if d.Timeline[1].Status != "pending" {
		t.Errorf("release[1] status = %q, want pending", d.Timeline[1].Status)
	}
	if len(d.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(d.Runs))
	}
	if d.Runs[0].SHA != "abcdef1" || d.Runs[0].Status != "success" || d.Runs[0].Duration != "1m 0s" {
		t.Errorf("run[0] wrong: %+v", d.Runs[0])
	}
	if d.Runs[1].Status != "failed" {
		t.Errorf("run[1] status = %s, want failed", d.Runs[1].Status)
	}
}

func TestMonitorHistory_SurfacesDeployLockAndClears(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production", ServerID: "s1", WorkflowFile: "mountabo-deploy-production.yml"},
	}}
	locker := NewDeployLocker()
	locker.TryAcquire(lockKey("acme", "shop", "production")) // a deploy to this environment is in flight
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, locker, nil)

	got, err := svc.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 || !got[0].Locked {
		t.Fatalf("want a locked deployment, got %+v", got)
	}
	if got[0].LockedSince == "" {
		t.Error("LockedSince should be set while locked")
	}

	// ClearLock frees it, and a subsequent read shows it unlocked.
	if !svc.ClearLock("acme", "shop", "production") {
		t.Error("ClearLock should report it cleared a held lock")
	}
	got, err = svc.History(context.Background())
	if err != nil {
		t.Fatalf("History after clear: %v", err)
	}
	if got[0].Locked {
		t.Error("deployment should be unlocked after ClearLock")
	}
}

// TestLiveURL_MatchesDeploymentsOwnDomain guards that a deployment's live URL is
// the domain whose upstream is ITS port — not just the server's first domain
// (a server can front several deployments, each with its own domain).
func TestLiveURL_MatchesDeploymentsOwnDomain(t *testing.T) {
	server := Server{IP: "203.0.113.7", Domains: []Domain{
		{Host: "other.example.com", Upstream: "8080"},
		{Host: "meals.example.com", Upstream: "9090"},
	}}
	if got := liveURL(server, 9090); got != "https://meals.example.com" {
		t.Errorf("liveURL(port 9090) = %q, want its own domain", got)
	}
	// A deployment with no recorded port (0) claims no domain — falls back to IP,
	// rather than borrowing the server's first domain.
	if got := liveURL(server, 0); got != "http://203.0.113.7" {
		t.Errorf("liveURL(port 0) = %q, want the server IP fallback", got)
	}
}

func TestMonitorSetPort_RecordsPortForAppEnvironment(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{App: "shop", Owner: "acme", Repo: "shop", Environment: "production", ServerID: "s1"},
	}}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)

	if err := svc.SetPort("acme", "shop", "production", 8010); err != nil {
		t.Fatalf("SetPort: %v", err)
	}
	if deps.saved[0].Port != 8010 {
		t.Errorf("recorded port = %d, want 8010", deps.saved[0].Port)
	}
	if err := svc.SetPort("acme", "shop", "staging", 8010); !errors.Is(err, ErrDeploymentNotFound) {
		t.Errorf("SetPort for a missing environment = %v, want ErrDeploymentNotFound", err)
	}
}

func TestMonitorHistory_EmptyWithoutDeployments(t *testing.T) {
	svc := NewMonitorService(&fakeDeploymentStore{}, fakeReleaseReader{}, &fakeDeploymentStore{}, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, &fakeDeploymentStore{}, NewDeployLocker(), nil)
	got, err := svc.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no deployments, got %d", len(got))
	}
}

func TestMonitorHistory_NotConnected(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{{Owner: "a", Repo: "r", Branch: "main"}}}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadErr: ErrNotConnected}, fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)
	if _, err := svc.History(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func TestMonitorDelete_TearsDownContainerWorkflowAndRecord(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production", ServerID: "s1", WorkflowFile: "mountabo-deploy-production.yml"},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", IP: "203.0.113.7", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY-PEM")
	teardown := &fakeContainerTeardown{}
	files := &fakeRepoFileDeleter{}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, servers, vault, teardown, files, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)

	if err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Teardown ran against the deployment's canonical identity, not its app name:
	// an app name is shared by every environment of the repo, so tearing down by
	// app could remove a sibling environment's containers.
	wantID := DeriveDeploymentID("acme", "shop", "production")
	if len(teardown.removed) != 1 || teardown.removed[0] != wantID {
		t.Errorf("container teardown = %v, want [%s]", teardown.removed, wantID)
	}
	// Both committed artifacts were deleted from the repo.
	wantFiles := []string{
		"acme/shop:.mountabo/deploy-" + DeriveDeploymentID("acme", "shop", "production") + ".sh",
		"acme/shop:.github/workflows/mountabo-deploy-production.yml",
	}
	if len(files.deleted) != len(wantFiles) {
		t.Fatalf("deleted files = %v, want %v", files.deleted, wantFiles)
	}
	for i, want := range wantFiles {
		if files.deleted[i] != want {
			t.Errorf("deleted[%d] = %q, want %q", i, files.deleted[i], want)
		}
	}
	// The record is gone.
	if got, _ := deps.List(); len(got) != 0 {
		t.Errorf("record still present after delete: %v", got)
	}
}

func TestMonitorDelete_NotFound(t *testing.T) {
	deps := &fakeDeploymentStore{}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)
	if err := svc.Delete(context.Background(), "acme", "shop", "missing", DeleteOptions{}, io.Discard); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("want ErrDeploymentNotFound, got %v", err)
	}
}

func TestMonitorDelete_RemovesRecordEvenWhenTeardownFails(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production", ServerID: "s1", WorkflowFile: "mountabo-deploy-production.yml"},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", IP: "203.0.113.7", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY-PEM")
	// Both the container removal and the repo file deletion fail; the record must
	// still be removed so the user is never stuck.
	teardown := &fakeContainerTeardown{err: errors.New("ssh down")}
	files := &fakeRepoFileDeleter{err: errors.New("github down")}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "tok"}}, fakeRunLister{}, servers, vault, teardown, files, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, NewDeployLocker(), nil)

	if err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard); err != nil {
		t.Fatalf("Delete should succeed despite soft teardown failures, got %v", err)
	}
	if got, _ := deps.List(); len(got) != 0 {
		t.Errorf("record still present after delete: %v", got)
	}
}

// TestDelete_KeepsSharedResourcesWhileASiblingEnvironmentExists is the "deleting
// one deployment cannot affect another" property at the mountabo level.
//
// The deploy key is per REPO, not per deployment. Removing it while another
// environment of the same repo still deploys would break that deployment's clone,
// on GitHub, on the box and in the vault at once.
func TestDelete_KeepsSharedResourcesWhileASiblingEnvironmentExists(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production",
			ServerID: "s1", WorkflowFile: "w1.yml", Config: &DeployConfig{DeployDir: "/opt/shop", DeployKeyFile: "k"}},
		{ID: "d2", App: "shop", Owner: "acme", Repo: "shop", Branch: "develop", Environment: "staging",
			ServerID: "s1", WorkflowFile: "w2.yml", Config: &DeployConfig{DeployDir: "/opt/shop", DeployKeyFile: "k"}},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", Name: "box", IP: "10.0.0.1", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")
	_ = vault.SaveSecret(deployKeyVaultKey("acme", "shop"), "DEPLOYKEY")

	purger := &fakeContainerTeardown{}
	keys := &fakeRepoKeyDeleter{}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRunLister{}, servers, vault, purger, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, keys, deps,
		NewDeployLocker(), testLogger())

	if err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(purger.specs) != 1 {
		t.Fatalf("want one purge, got %d", len(purger.specs))
	}
	if purger.specs[0].RemoveDeployKey {
		t.Error("the deploy key must be kept on the box while staging still uses it")
	}
	if len(keys.cleared) != 0 {
		t.Error("the GitHub deploy key must be kept while staging still uses it")
	}
	if v, err := vault.LoadSecret(deployKeyVaultKey("acme", "shop")); err != nil || v == "" {
		t.Error("the vault copy of the deploy key must be kept while staging still uses it")
	}
	// The server-side purge is still scoped to the deleted deployment only.
	if got := purger.specs[0].DeploymentID; got != DeriveDeploymentID("acme", "shop", "production") {
		t.Errorf("purge targeted %q, not the deployment being deleted", got)
	}
}

// TestDelete_RemovesSharedResourcesForTheLastEnvironment is the other half: once
// nothing else needs the key, leaving it behind is an orphaned live credential.
func TestDelete_RemovesSharedResourcesForTheLastEnvironment(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production",
			ServerID: "s1", WorkflowFile: "w1.yml", Config: &DeployConfig{DeployDir: "/opt/shop", DeployKeyFile: "k"}},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", Name: "box", IP: "10.0.0.1", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")
	_ = vault.SaveSecret(deployKeyVaultKey("acme", "shop"), "DEPLOYKEY")

	purger := &fakeContainerTeardown{}
	keys := &fakeRepoKeyDeleter{}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRunLister{}, servers, vault, purger, &fakeRepoFileDeleter{}, &fakeRepoEnvDeleter{}, keys, deps,
		NewDeployLocker(), testLogger())

	if err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !purger.specs[0].RemoveDeployKey {
		t.Error("the deploy key should be removed from the box once nothing else needs it")
	}
	if len(keys.cleared) == 0 {
		t.Error("the GitHub deploy key should be removed for the last environment")
	}
}

// TestDelete_DisablesTheTriggerBeforeTouchingTheServer keeps a push landing
// mid-purge from recreating what is being removed.
func TestDelete_DisablesTheTriggerBeforeTouchingTheServer(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production",
			ServerID: "s1", WorkflowFile: "w1.yml", Config: &DeployConfig{DeployDir: "/opt/shop"}},
	}}
	servers := newMemServerStore()
	_ = servers.Save(Server{ID: "s1", Name: "box", IP: "10.0.0.1", SSHPort: 22, Status: StatusReady})
	vault := newFakeVault()
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")

	order := &orderRecorder{}
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRunLister{}, servers, vault, order, order, &fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps,
		NewDeployLocker(), testLogger())

	if err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(order.events) < 2 || order.events[0] != "workflow" {
		t.Errorf("the trigger must be disabled first, got order %v", order.events)
	}
}

// orderRecorder records the sequence of teardown steps.
type orderRecorder struct{ events []string }

func (o *orderRecorder) PurgeApp(_ context.Context, _ SSHTarget, _ PurgeSpec, _ io.Writer) error {
	o.events = append(o.events, "purge")
	return nil
}

func (o *orderRecorder) DeleteFile(_ context.Context, _ Token, _, _, _, _, _ string) error {
	o.events = append(o.events, "workflow")
	return nil
}

// TestDelete_HoldsTheDeployLock keeps a delete from racing a deploy mountabo
// started against the very resources being removed.
func TestDelete_HoldsTheDeployLock(t *testing.T) {
	deps := &fakeDeploymentStore{saved: []Deployment{
		{ID: "d1", App: "shop", Owner: "acme", Repo: "shop", Branch: "main", Environment: "production",
			ServerID: "s1", WorkflowFile: "w1.yml", Config: &DeployConfig{DeployDir: "/opt/shop"}},
	}}
	locker := NewDeployLocker()
	svc := NewMonitorService(deps, fakeReleaseReader{}, deps, &fakeStore{loadTok: Token{AccessToken: "t"}},
		fakeRunLister{}, newMemServerStore(), newFakeVault(), &fakeContainerTeardown{}, &fakeRepoFileDeleter{},
		&fakeRepoEnvDeleter{}, &fakeRepoKeyDeleter{}, deps, locker, testLogger())

	locker.TryAcquire(lockKey("acme", "shop", "production"))
	err := svc.Delete(context.Background(), "acme", "shop", "production", DeleteOptions{}, io.Discard)
	if !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("want ErrDeployInProgress while a deploy holds the lock, got %v", err)
	}
}

// testLogger discards output: these tests assert behaviour, and the teardown path
// deliberately logs a great deal about what it kept and why.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
