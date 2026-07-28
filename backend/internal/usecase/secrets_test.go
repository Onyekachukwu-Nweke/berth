package usecase

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSecretLister returns canned GitHub secret infos (what is configured).
type fakeSecretLister struct {
	infos []SecretInfo
	err   error
}

func (f fakeSecretLister) ListEnvSecrets(_ context.Context, _ Token, _, _, _ string) ([]SecretInfo, error) {
	return f.infos, f.err
}

// fakeSecretDeleter records the names it was asked to delete from GitHub.
type fakeSecretDeleter struct {
	deleted []string
	err     error
}

func (f *fakeSecretDeleter) DeleteEnvSecret(_ context.Context, _ Token, _, _, _, name string) error {
	f.deleted = append(f.deleted, name)
	return f.err
}

// fakeSecretStore is an in-memory SecretStore keyed by owner/repo/environment.
type fakeSecretStore struct {
	data    map[string]map[string]SecretValue
	saveErr error
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: map[string]map[string]SecretValue{}}
}

func (f *fakeSecretStore) key(owner, repo, environment string) string {
	return owner + "/" + repo + "/" + environment
}

func (f *fakeSecretStore) SaveSecretValues(owner, repo, environment string, values map[string]SecretValue) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	k := f.key(owner, repo, environment)
	if f.data[k] == nil {
		f.data[k] = map[string]SecretValue{}
	}
	for name, v := range values {
		f.data[k][name] = v
	}
	return nil
}

func (f *fakeSecretStore) LoadSecretValues(owner, repo, environment string) (map[string]SecretValue, error) {
	out := map[string]SecretValue{}
	for name, v := range f.data[f.key(owner, repo, environment)] {
		out[name] = v
	}
	return out, nil
}

func (f *fakeSecretStore) DeleteSecretValue(owner, repo, environment, name string) error {
	delete(f.data[f.key(owner, repo, environment)], name)
	return nil
}

func (f *fakeSecretStore) ClearSecretValues(owner, repo, environment string) error {
	delete(f.data, f.key(owner, repo, environment))
	return nil
}

// fakeExampleReader returns canned .env.example keys.
type fakeExampleReader struct {
	keys []string
	err  error
}

func (f fakeExampleReader) EnvExampleKeys(_ context.Context, _ Token, _ RepoRef) ([]string, error) {
	return f.keys, f.err
}

func TestSecretsList_ClassifiesMergesAndFlagsDrift(t *testing.T) {
	seen := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	changed := seen.Add(time.Hour) // GitHub updated it after mountabo last synced

	lister := fakeSecretLister{infos: []SecretInfo{
		{Name: "SERVER_HOST", UpdatedAt: seen},   // managed
		{Name: "DATABASE_URL", UpdatedAt: seen},  // operator, in sync
		{Name: "API_KEY", UpdatedAt: changed},    // operator, changed on GitHub
		{Name: "SET_ELSEWHERE", UpdatedAt: seen}, // operator, no local value
	}}
	store := newFakeSecretStore()
	store.data[store.key("acme", "shop", "production")] = map[string]SecretValue{
		"DATABASE_URL": {Value: "pg://x", GitHubUpdatedAtSeen: seen},
		"API_KEY":      {Value: "sk-1", GitHubUpdatedAtSeen: seen},
		"REMOVED":      {Value: "old", GitHubUpdatedAtSeen: seen}, // gone from GitHub
	}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}},
		&fakeDeploymentStore{},
		lister, &fakeSecretSetter{}, &fakeSecretDeleter{}, store, fakeExampleReader{},
	)

	view, err := svc.List(context.Background(), "acme", "shop", "production", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]SecretView{}
	for _, v := range view.Secrets {
		byName[v.Name] = v
	}
	if v := byName["SERVER_HOST"]; !v.Managed || v.Status != "" || v.Value != "" {
		t.Errorf("SERVER_HOST should be managed, unflagged, valueless: %+v", v)
	}
	if v := byName["DATABASE_URL"]; v.Managed || v.Status != "" || v.Value != "pg://x" || !v.HasLocalValue {
		t.Errorf("DATABASE_URL should be a clean operator secret with its value: %+v", v)
	}
	if v := byName["API_KEY"]; v.Status != SecretStale {
		t.Errorf("API_KEY should be stale, got %q", v.Status)
	}
	if v := byName["SET_ELSEWHERE"]; v.Status != SecretSetOutsideMountabo || v.HasLocalValue {
		t.Errorf("SET_ELSEWHERE should be set-outside with no local value: %+v", v)
	}
	if v := byName["REMOVED"]; v.Status != SecretRemovedOnGitHub {
		t.Errorf("REMOVED should be removed-on-GitHub, got %q", v.Status)
	}
}

func TestSecretsList_OmitsValuesWhenNotRequested(t *testing.T) {
	store := newFakeSecretStore()
	store.data[store.key("acme", "shop", "production")] = map[string]SecretValue{
		"DATABASE_URL": {Value: "pg://x"},
	}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}}},
		&fakeSecretSetter{}, &fakeSecretDeleter{}, store, fakeExampleReader{},
	)
	view, err := svc.List(context.Background(), "acme", "shop", "production", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if v := view.Secrets[0]; v.Value != "" || !v.HasLocalValue {
		t.Errorf("compare view must know the value exists but not expose it: %+v", v)
	}
}

func TestSecretsList_DetectsMissingFromEnvExample(t *testing.T) {
	deployments := &fakeDeploymentStore{saved: []Deployment{
		{Owner: "acme", Repo: "shop", Environment: "production", Branch: "main", RootDir: "apps/api"},
	}}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, deployments,
		fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}}},
		&fakeSecretSetter{}, &fakeSecretDeleter{}, newFakeSecretStore(),
		fakeExampleReader{keys: []string{"DATABASE_URL", "REDIS_URL", "STRIPE_KEY"}},
	)
	view, err := svc.List(context.Background(), "acme", "shop", "production", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, m := range view.Missing {
		got[m] = true
	}
	if len(view.Missing) != 2 || !got["REDIS_URL"] || !got["STRIPE_KEY"] {
		t.Errorf("missing = %v, want REDIS_URL + STRIPE_KEY only", view.Missing)
	}
}

func TestSecretsSet_WritesGitHubAndRecordsLocally(t *testing.T) {
	setter := &fakeSecretSetter{}
	store := newFakeSecretStore()
	setAt := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	// The post-write re-list reports the GitHub updated-at mountabo should record.
	lister := fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL", UpdatedAt: setAt}}}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		lister, setter, &fakeSecretDeleter{}, store, fakeExampleReader{},
	)

	result, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "DATABASE_URL", Value: "pg://new"}})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if result.Partial {
		t.Errorf("Set should fully sync, got partial: %+v", result)
	}
	if setter.env != "production" || len(setter.secrets) != 1 || setter.secrets[0].Value != "pg://new" {
		t.Errorf("GitHub write wrong: %+v", setter)
	}
	stored := store.data[store.key("acme", "shop", "production")]["DATABASE_URL"]
	if stored.Value != "pg://new" || !stored.GitHubUpdatedAtSeen.Equal(setAt) {
		t.Errorf("local record wrong: %+v", stored)
	}
}

func TestSecretsSet_RejectsManagedName(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, setter, &fakeSecretDeleter{}, newFakeSecretStore(), fakeExampleReader{},
	)
	if _, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "SERVER_SSH_KEY", Value: "x"}}); !errors.Is(err, ErrManagedSecret) {
		t.Fatalf("want ErrManagedSecret, got %v", err)
	}
	if setter.env != "" {
		t.Error("a managed name must be rejected before any GitHub write")
	}
}

func TestSecretsSet_PartialWhenLocalStoreFails(t *testing.T) {
	store := newFakeSecretStore()
	store.saveErr = errors.New("disk full")
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}}},
		&fakeSecretSetter{}, &fakeSecretDeleter{}, store, fakeExampleReader{},
	)
	result, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "DATABASE_URL", Value: "pg://new"}})
	if err != nil {
		t.Fatalf("a local-store failure after a GitHub write must not be a hard error: %v", err)
	}
	if !result.Partial || result.Message == "" {
		t.Errorf("want a partial result with a message, got %+v", result)
	}
}

func TestSecretsDelete_RejectsManagedAndClearsLocal(t *testing.T) {
	deleter := &fakeSecretDeleter{}
	store := newFakeSecretStore()
	store.data[store.key("acme", "shop", "production")] = map[string]SecretValue{"DATABASE_URL": {Value: "pg://x"}}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, &fakeSecretSetter{}, deleter, store, fakeExampleReader{},
	)

	if err := svc.Delete(context.Background(), "acme", "shop", "production", "DEPLOY_DIR"); !errors.Is(err, ErrManagedSecret) {
		t.Fatalf("want ErrManagedSecret for managed name, got %v", err)
	}
	if len(deleter.deleted) != 0 {
		t.Error("a managed name must be rejected before any GitHub delete")
	}

	if err := svc.Delete(context.Background(), "acme", "shop", "production", "DATABASE_URL"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleter.deleted) != 1 || deleter.deleted[0] != "DATABASE_URL" {
		t.Errorf("GitHub delete not invoked as expected: %+v", deleter.deleted)
	}
	if _, ok := store.data[store.key("acme", "shop", "production")]["DATABASE_URL"]; ok {
		t.Error("local value should be cleared after delete")
	}
}
