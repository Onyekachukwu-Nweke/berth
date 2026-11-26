package usecase

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type putCall struct{ branch, path, content string }

type fakeRepoWriter struct {
	calls []putCall
	err   error
}

func (f *fakeRepoWriter) PutFile(_ context.Context, _ Token, _, _, branch, path, content, _ string) error {
	f.calls = append(f.calls, putCall{branch: branch, path: path, content: content})
	return f.err
}

type fakeEnvManager struct {
	env string
	err error
}

func (f *fakeEnvManager) EnsureEnvironment(_ context.Context, _ Token, _, _, name string) error {
	f.env = name
	return f.err
}

type fakeSecretSetter struct {
	env     string
	secrets []NamedSecret
	err     error
}

func (f *fakeSecretSetter) SetEnvSecrets(_ context.Context, _ Token, _, _, environment string, secrets []NamedSecret) error {
	f.env, f.secrets = environment, secrets
	return f.err
}

type fakeDeploymentStore struct{ saved []Deployment }

func (f *fakeDeploymentStore) List() ([]Deployment, error) { return f.saved, nil }
func (f *fakeDeploymentStore) Save(d Deployment) error {
	f.saved = append(f.saved, d)
	return nil
}

// SetPort updates the recorded port of the matching deployment, so the store
// doubles as a DeploymentPortSetter in tests.
func (f *fakeDeploymentStore) SetPort(owner, repo, environment string, port int) (bool, error) {
	updated := false
	for i, d := range f.saved {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			f.saved[i].Port = port
			updated = true
		}
	}
	return updated, nil
}

// Delete removes every saved deployment matching (owner, repo, environment),
// reporting whether any was removed, so the store doubles as a DeploymentDeleter
// in tests.
func (f *fakeDeploymentStore) Delete(owner, repo, environment string) (bool, error) {
	kept := make([]Deployment, 0, len(f.saved))
	removed := false
	for _, d := range f.saved {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			removed = true
			continue
		}
		kept = append(kept, d)
	}
	f.saved = kept
	return removed, nil
}

// fakeRefResolver returns a canned head SHA (or an error) so a test can assert
// the resolved commit lands on the recorded release and that a resolve failure
// does not fail the deploy.
type fakeRefResolver struct {
	sha string
	err error
}

func (f fakeRefResolver) HeadSHA(_ context.Context, _ Token, _, _, _ string) (string, error) {
	return f.sha, f.err
}

// fakeWorkflowDispatcher records the workflow_dispatch calls a re-apply made, so
// a test can assert a redeploy was (or wasn't) triggered.
type fakeWorkflowDispatcher struct {
	dispatched []string // "owner/repo:workflowFile@ref"
	err        error
}

func (f *fakeWorkflowDispatcher) Dispatch(_ context.Context, _ Token, owner, repo, workflowFile, ref string) error {
	f.dispatched = append(f.dispatched, owner+"/"+repo+":"+workflowFile+"@"+ref)
	return f.err
}

type fakeTokenStore struct {
	token Token
	err   error
}

func (f fakeTokenStore) TokenFor(context.Context, string) (Token, error) { return f.token, f.err }

func readyDeployFixture(t *testing.T) (*memServerStore, *fakeVault, *fakeRepoWriter, *fakeEnvManager, *fakeSecretSetter, *DeployService) {
	t.Helper()
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
	repo, envs, secrets := &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, repo, envs, secrets, &fakeDeploymentStore{}, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())
	return store, vault, repo, envs, secrets, svc
}

func deployInput() DeployInput {
	return DeployInput{
		ServerID:  "s1",
		App:       "shop",
		Owner:     "acme",
		Repo:      "shop",
		Branch:    "main",
		DeployDir: "/opt/shop",
		Ports:     []DeployPort{{EnvVar: "FRONTEND_PORT", Value: "3000", Container: "3000"}},
		EnvVars:   []DeployEnvVar{{Key: "DATABASE_URL", Value: "postgres://secret"}},
	}
}

func TestDeploy_WritesArtifactsProvisionsEnvAndSecrets(t *testing.T) {
	_, _, repo, envs, secrets, svc := readyDeployFixture(t)

	var out strings.Builder
	if err := svc.Deploy(context.Background(), deployInput(), &out); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Two commits: the per-deployment script, then the per-environment workflow.
	if len(repo.calls) != 2 {
		t.Fatalf("want 2 file writes, got %d", len(repo.calls))
	}
	// Scoped to the deployment id, not a shared repo-root deploy.sh, so a second
	// environment of this repo cannot overwrite it.
	wantScript := ".mountabo/deploy-" + DeriveDeploymentID("acme", "shop", "main") + ".sh"
	if repo.calls[0].path != wantScript {
		t.Errorf("first write path = %q, want %q", repo.calls[0].path, wantScript)
	}
	if repo.calls[1].path != ".github/workflows/mountabo-deploy-main.yml" {
		t.Errorf("second write path = %q", repo.calls[1].path)
	}
	for _, c := range repo.calls {
		if c.branch != "main" {
			t.Errorf("write to branch %q, want main", c.branch)
		}
	}

	// Environment defaults to the branch.
	if envs.env != "main" || secrets.env != "main" {
		t.Errorf("environment = %q/%q, want main", envs.env, secrets.env)
	}

	// Managed connection secrets + the operator's env var, with the right values.
	want := map[string]string{
		"SERVER_HOST":    "5.6.7.8",
		"SERVER_USER":    BootstrapUser,
		"SERVER_SSH_KEY": "BERTH-KEY-PEM",
		"DEPLOY_DIR":     "/opt/shop",
		"DATABASE_URL":   "postgres://secret",
	}
	got := map[string]string{}
	for _, s := range secrets.secrets {
		got[s.Name] = s.Value
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("secret %s = %q, want %q", name, got[name], value)
		}
	}

	// Secret values must never reach the live output stream.
	if log := out.String(); strings.Contains(log, "BERTH-KEY-PEM") || strings.Contains(log, "postgres://secret") {
		t.Errorf("secret value leaked into stream output:\n%s", log)
	}
}

func TestDeploy_RejectsUnsafeEnvironmentName(t *testing.T) {
	_, _, _, _, _, svc := readyDeployFixture(t)
	in := deployInput()
	in.Environment = "feature/foo" // a slash would break the /{environment} route
	if err := svc.Deploy(context.Background(), in, new(strings.Builder)); err == nil {
		t.Fatal("Deploy should reject an environment name with a slash")
	}

	// A blank environment that would default to a slashed branch is also rejected.
	in2 := deployInput()
	in2.Environment = ""
	in2.Branch = "feature/foo"
	if err := svc.Deploy(context.Background(), in2, new(strings.Builder)); err == nil {
		t.Fatal("Deploy should reject a blank environment that defaults to a slashed branch")
	}
}

func TestDeploy_EnvironmentOverridesBranch(t *testing.T) {
	_, _, _, envs, secrets, svc := readyDeployFixture(t)
	in := deployInput()
	in.Environment = "production"

	if err := svc.Deploy(context.Background(), in, new(strings.Builder)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if envs.env != "production" || secrets.env != "production" {
		t.Errorf("environment = %q/%q, want production", envs.env, secrets.env)
	}
}

func TestDeploy_RecordsReleaseCommitAndActor(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
	history := &fakeDeploymentStore{}
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, history, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())

	if err := svc.Deploy(WithUser(context.Background(), User{Login: "octocat"}), deployInput(), new(strings.Builder)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(history.saved) != 1 {
		t.Fatalf("want 1 recorded release, got %d", len(history.saved))
	}
	if rec := history.saved[0]; rec.CommitSHA != "abc1234def" || rec.Actor != "octocat" {
		t.Errorf("release commit/actor = %q/%q, want abc1234def/octocat", rec.CommitSHA, rec.Actor)
	}
}

func TestDeploy_ReleaseMetadataFailureIsNonFatal(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
	history := &fakeDeploymentStore{}
	// Both the commit resolve and the actor lookup fail; the deploy must still
	// succeed and record a release, just with the fields left empty.
	metaErr := errors.New("github unavailable")
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, history, &fakeRepoKeyDeleter{}, fakeRefResolver{err: metaErr}, &fakeWorkflowDispatcher{}, NewDeployLocker())

	if err := svc.Deploy(context.Background(), deployInput(), new(strings.Builder)); err != nil {
		t.Fatalf("Deploy should not fail on release-metadata errors: %v", err)
	}
	if len(history.saved) != 1 {
		t.Fatalf("want 1 recorded release, got %d", len(history.saved))
	}
	if rec := history.saved[0]; rec.CommitSHA != "" || rec.Actor != "" {
		t.Errorf("release commit/actor = %q/%q, want both empty", rec.CommitSHA, rec.Actor)
	}
}

func TestDeploy_LockRejectsConcurrentAndReleasesAfter(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
	locker := NewDeployLocker()
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, &fakeDeploymentStore{}, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, locker)
	in := deployInput()

	// A deploy to the same app is in flight (lock held): the second is rejected.
	if !locker.TryAcquire(lockKey(in.Owner, in.Repo, in.Branch)) {
		t.Fatal("precondition: could not acquire the lock")
	}
	if err := svc.Deploy(context.Background(), in, new(strings.Builder)); !errors.Is(err, ErrDeployInProgress) {
		t.Fatalf("concurrent deploy: want ErrDeployInProgress, got %v", err)
	}
	locker.Release(lockKey(in.Owner, in.Repo, in.Branch))

	// With the lock free, the deploy succeeds and releases the lock on return.
	if err := svc.Deploy(context.Background(), in, new(strings.Builder)); err != nil {
		t.Fatalf("Deploy after release: %v", err)
	}
	if _, held := locker.Held(lockKey(in.Owner, in.Repo, in.Branch)); held {
		t.Error("lock should be released after Deploy returns")
	}
}

func TestDeploy_RequiresReadyServer(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", Status: StatusProbed})
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, &fakeDeploymentStore{}, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())

	if err := svc.Deploy(context.Background(), deployInput(), new(strings.Builder)); err == nil {
		t.Fatal("expected an error deploying to a server that is not ready")
	}
}

func TestDeploy_RejectsInvalidEnvVarName(t *testing.T) {
	_, _, repo, _, _, svc := readyDeployFixture(t)
	in := deployInput()
	in.EnvVars = []DeployEnvVar{{Key: "9bad name", Value: "x"}}

	if err := svc.Deploy(context.Background(), in, new(strings.Builder)); err == nil {
		t.Fatal("expected an error for an invalid env var name")
	}
	if len(repo.calls) != 0 {
		t.Errorf("nothing should be written when validation fails, got %d writes", len(repo.calls))
	}
}

func TestDeploy_NotConnected(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")
	svc := NewDeployService(store, vault, fakeTokenStore{err: ErrNotConnected}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, &fakeDeploymentStore{}, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())

	err := svc.Deploy(context.Background(), deployInput(), new(strings.Builder))
	if err == nil {
		t.Fatal("expected ErrNotConnected to propagate")
	}
}

func TestPreview_GeneratesArtifactsWithoutSideEffects(t *testing.T) {
	_, _, repo, envs, secrets, svc := readyDeployFixture(t)

	art, err := svc.Preview(deployInput())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if art.WorkflowPath != ".github/workflows/mountabo-deploy-main.yml" {
		t.Errorf("workflow path = %q", art.WorkflowPath)
	}
	if !strings.Contains(art.DeployScript, "docker compose $COMPOSE_ARGS up -d --no-build") {
		t.Error("expected a compose deploy script by default")
	}
	if !strings.Contains(art.Workflow, "environment: main") {
		t.Error("expected the workflow to pin the environment")
	}
	names := map[string]bool{}
	for _, s := range art.Secrets {
		names[s.Name] = true
	}
	for _, n := range []string{"SERVER_HOST", "SERVER_SSH_KEY", "DEPLOY_DIR", "DATABASE_URL"} {
		if !names[n] {
			t.Errorf("preview secrets missing %s", n)
		}
	}
	// Preview is pure: nothing is committed, no environment or secrets are set.
	if len(repo.calls) != 0 || envs.env != "" || len(secrets.secrets) != 0 {
		t.Error("Preview must have no side effects")
	}
}

func TestPreview_RejectsInvalidEnvVarName(t *testing.T) {
	_, _, _, _, _, svc := readyDeployFixture(t)
	in := deployInput()
	in.EnvVars = []DeployEnvVar{{Key: "bad name", Value: "x"}}
	if _, err := svc.Preview(in); err == nil {
		t.Fatal("expected an error for an invalid env var name")
	}
}

// Nothing is cloned onto servers any more, so a deploy never registers or
// installs a deploy key. It names the old key file so deploy.sh removes it.
func TestDeploy_NoDeployKeyAndNoClone(t *testing.T) {
	_, vault, repo, _, _, svc := readyDeployFixture(t)
	if err := svc.Deploy(context.Background(), deployInput(), io.Discard); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if _, ok := vault.secrets[deployKeyVaultKey("acme", "shop")]; ok {
		t.Error("a deploy must not create a deploy key")
	}
	var script string
	for _, c := range repo.calls {
		if strings.HasPrefix(c.path, ".mountabo/") {
			script = c.content
		}
	}
	if strings.Contains(script, "git clone") || strings.Contains(script, "GIT_SSH_COMMAND") {
		t.Error("deploy.sh must not clone the repository")
	}
	if !strings.Contains(script, `rm -f "$HOME/.ssh/mountabo_deploy_acme_shop"`) {
		t.Error("deploy.sh should remove the old deploy key from the server")
	}
}

// The old deploy key is removed from the repo and the vault on deploy, but
// only when no other environment of the repo might still clone with it.
func TestDeploy_RetiresTheOldDeployKeyOnlyWhenSafe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		siblings []Deployment
		retired  bool
	}{
		{"only environment", nil, true},
		{"same environment redeployed", []Deployment{{Owner: "acme", Repo: "shop", Environment: "main"}}, true},
		{"another environment not redeployed yet", []Deployment{{Owner: "acme", Repo: "shop", Environment: "staging"}}, false},
	} {
		store, vault := newMemServerStore(), newFakeVault()
		_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
		_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
		_ = vault.SaveSecret(deployKeyVaultKey("acme", "shop"), "OLD-DEPLOY-KEY")
		keys := &fakeRepoKeyDeleter{}
		svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, &fakeRepoWriter{}, &fakeEnvManager{}, &fakeSecretSetter{}, &fakeDeploymentStore{saved: tc.siblings}, keys, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())
		if err := svc.Deploy(context.Background(), deployInput(), io.Discard); err != nil {
			t.Fatalf("%s: Deploy: %v", tc.name, err)
		}
		_, stillStored := vault.secrets[deployKeyVaultKey("acme", "shop")]
		if tc.retired && (len(keys.cleared) != 1 || stillStored) {
			t.Errorf("%s: want the key removed from the repo and vault (calls=%v, stored=%v)", tc.name, keys.cleared, stillStored)
		}
		if !tc.retired && (len(keys.cleared) != 0 || !stillStored) {
			t.Errorf("%s: want the key kept for the other environment (calls=%v, stored=%v)", tc.name, keys.cleared, stillStored)
		}
	}
}

// reapplyService builds a DeployService wired for ReapplyWorkflow tests: the repo
// writer records PutFile calls, the dispatcher records dispatches, and the store
// is seeded with the given deployment record.
func reapplyService(dep Deployment) (*fakeRepoWriter, *fakeWorkflowDispatcher, *DeployService) {
	repo := &fakeRepoWriter{}
	disp := &fakeWorkflowDispatcher{}
	store := &fakeDeploymentStore{saved: []Deployment{dep}}
	svc := NewDeployService(newMemServerStore(), newFakeVault(), fakeTokenStore{token: Token{AccessToken: "tok"}}, repo, &fakeEnvManager{}, &fakeSecretSetter{}, store, &fakeRepoKeyDeleter{}, fakeRefResolver{}, disp, NewDeployLocker())
	return repo, disp, svc
}

func TestReapplyWorkflow_LegacyDeploymentIsRejected(t *testing.T) {
	// No persisted Config → legacy: can't regenerate, must not commit or dispatch.
	repo, disp, svc := reapplyService(Deployment{
		Owner: "acme", Repo: "shop", Environment: "production", Branch: "main",
		App: "shop", WorkflowFile: "mountabo-deploy-production.yml",
	})
	err := svc.ReapplyWorkflow(context.Background(), Token{AccessToken: "tok"}, "acme", "shop", "production", []string{"API_KEY"})
	if !errors.Is(err, ErrLegacyDeployment) {
		t.Fatalf("want ErrLegacyDeployment, got %v", err)
	}
	if len(repo.calls) != 0 {
		t.Errorf("legacy re-apply must not commit anything, got %+v", repo.calls)
	}
	if len(disp.dispatched) != 0 {
		t.Errorf("legacy re-apply must not dispatch, got %+v", disp.dispatched)
	}
}

func TestReapplyWorkflow_RegeneratesBothAndDispatches(t *testing.T) {
	repo, disp, svc := reapplyService(Deployment{
		Owner: "acme", Repo: "shop", Environment: "production", Branch: "main",
		App: "shop", WorkflowFile: "mountabo-deploy-production.yml",
		Config: &DeployConfig{Strategy: "compose", DeployDir: "/opt/shop"},
	})
	if err := svc.ReapplyWorkflow(context.Background(), Token{AccessToken: "tok"}, "acme", "shop", "production", []string{"API_KEY"}); err != nil {
		t.Fatalf("ReapplyWorkflow: %v", err)
	}
	// Commits both the deploy script and the workflow.
	var paths []string
	for _, c := range repo.calls {
		paths = append(paths, c.path)
	}
	wantScript := ".mountabo/deploy-" + DeriveDeploymentID("acme", "shop", "production") + ".sh"
	if len(paths) != 2 || paths[0] != wantScript || paths[1] != ".github/workflows/mountabo-deploy-production.yml" {
		t.Errorf("re-apply should commit %s + workflow, got %v", wantScript, paths)
	}
	// The regenerated workflow carries the new key.
	if !strings.Contains(repo.calls[1].content, "API_KEY") {
		t.Errorf("regenerated workflow should reference API_KEY:\n%s", repo.calls[1].content)
	}
	// And a redeploy is dispatched for the workflow on its branch.
	want := "acme/shop:mountabo-deploy-production.yml@main"
	if len(disp.dispatched) != 1 || disp.dispatched[0] != want {
		t.Errorf("dispatch = %v, want [%s]", disp.dispatched, want)
	}
}

// orderLog records the deploy's side effects in the order they happen.
type orderLog struct{ events []string }

type orderedRepo struct{ log *orderLog }

func (o orderedRepo) PutFile(_ context.Context, _ Token, _, _, _, path, _, _ string) error {
	o.log.events = append(o.log.events, "commit "+path)
	return nil
}

type orderedEnvs struct{ log *orderLog }

func (o orderedEnvs) EnsureEnvironment(_ context.Context, _ Token, _, _, _ string) error {
	o.log.events = append(o.log.events, "environment")
	return nil
}

type orderedSecrets struct{ log *orderLog }

func (o orderedSecrets) SetEnvSecrets(_ context.Context, _ Token, _, _, _ string, _ []NamedSecret) error {
	o.log.events = append(o.log.events, "secrets")
	return nil
}

// The workflow commit starts a run immediately and GitHub hands a job its
// secrets when it starts, so the environment and its secrets must exist before
// anything is committed, and the workflow must be the last commit.
func TestDeploy_SecretsExistBeforeTheWorkflowIsCommitted(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "BERTH-KEY-PEM")
	log := &orderLog{}
	svc := NewDeployService(store, vault, fakeTokenStore{token: Token{AccessToken: "tok"}}, orderedRepo{log}, orderedEnvs{log}, orderedSecrets{log}, &fakeDeploymentStore{}, &fakeRepoKeyDeleter{}, fakeRefResolver{sha: "abc1234def"}, &fakeWorkflowDispatcher{}, NewDeployLocker())

	if err := svc.Deploy(context.Background(), deployInput(), io.Discard); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	pos := map[string]int{}
	for i, e := range log.events {
		if _, seen := pos[e]; !seen {
			pos[e] = i
		}
	}
	firstCommit := len(log.events)
	for i, e := range log.events {
		if strings.HasPrefix(e, "commit ") {
			firstCommit = i
			break
		}
	}
	if pos["environment"] > firstCommit || pos["secrets"] > firstCommit {
		t.Fatalf("environment/secrets must come before any commit, got %v", log.events)
	}
	last := log.events[len(log.events)-1]
	if !strings.HasPrefix(last, "commit .github/workflows/") {
		t.Fatalf("the workflow must be the last commit, got %v", log.events)
	}
}
