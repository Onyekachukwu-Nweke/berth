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

// fakeExampleReader returns canned .env.example keys.
type fakeExampleReader struct {
	keys []string
	err  error
}

func (f fakeExampleReader) EnvExampleKeys(_ context.Context, _ Token, _ RepoRef) ([]string, error) {
	return f.keys, f.err
}

// fakeReapplier records the env-key set a re-apply was asked to redeploy with,
// and can return a canned error (e.g. ErrLegacyDeployment) to exercise the
// ApplyResult mapping.
type fakeReapplier struct {
	gotKeys []string
	called  bool
	err     error
}

func (f *fakeReapplier) ReapplyWorkflow(_ context.Context, _ Token, _, _, _ string, envKeys []string) error {
	f.called = true
	f.gotKeys = envKeys
	return f.err
}

func newSecretsSvc(lister EnvSecretLister, setter EnvSecretSetter, deleter EnvSecretDeleter, example EnvExampleReader, reapplier DeployReapplier, deployments DeploymentStore) *SecretsService {
	return NewSecretsService(&fakeStore{loadTok: Token{AccessToken: "tok"}}, deployments, lister, setter, deleter, example, reapplier)
}

func TestSecretsList_ClassifiesNamesAndUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lister := fakeSecretLister{infos: []SecretInfo{
		{Name: "SERVER_HOST", UpdatedAt: updated},  // managed
		{Name: "DATABASE_URL", UpdatedAt: updated}, // operator
	}}
	svc := newSecretsSvc(lister, &fakeSecretSetter{}, &fakeSecretDeleter{}, fakeExampleReader{}, &fakeReapplier{}, &fakeDeploymentStore{})

	view, err := svc.List(context.Background(), "acme", "shop", "production")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]SecretView{}
	for _, v := range view.Secrets {
		byName[v.Name] = v
	}
	if v := byName["SERVER_HOST"]; !v.Managed || !v.UpdatedAt.Equal(updated) {
		t.Errorf("SERVER_HOST should be managed with its updatedAt: %+v", v)
	}
	if v := byName["DATABASE_URL"]; v.Managed || !v.UpdatedAt.Equal(updated) {
		t.Errorf("DATABASE_URL should be an operator secret: %+v", v)
	}
}

func TestSecretsList_DetectsMissingFromEnvExample(t *testing.T) {
	deployments := &fakeDeploymentStore{saved: []Deployment{
		{Owner: "acme", Repo: "shop", Environment: "production", Branch: "main", RootDir: "apps/api"},
	}}
	svc := newSecretsSvc(
		fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}}},
		&fakeSecretSetter{}, &fakeSecretDeleter{},
		fakeExampleReader{keys: []string{"DATABASE_URL", "REDIS_URL", "STRIPE_KEY"}},
		&fakeReapplier{}, deployments,
	)
	view, err := svc.List(context.Background(), "acme", "shop", "production")
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

func TestSecretsSet_WritesGitHubAndReapplies(t *testing.T) {
	setter := &fakeSecretSetter{}
	reapplier := &fakeReapplier{}
	// GitHub already lists DATABASE_URL + a managed one; the overlay adds the just-
	// set API_KEY and excludes the managed name.
	lister := fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}, {Name: "SERVER_HOST"}}}
	svc := newSecretsSvc(lister, setter, &fakeSecretDeleter{}, fakeExampleReader{}, reapplier, &fakeDeploymentStore{})

	result, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "API_KEY", Value: "sk-1"}})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if setter.env != "production" || len(setter.secrets) != 1 || setter.secrets[0].Value != "sk-1" {
		t.Errorf("GitHub write wrong: %+v", setter)
	}
	if !result.Redeployed {
		t.Errorf("want redeployed on a successful re-apply, got %+v", result)
	}
	// Desired keys = (list ∪ added) \ managed, sorted.
	want := []string{"API_KEY", "DATABASE_URL"}
	if len(reapplier.gotKeys) != len(want) || reapplier.gotKeys[0] != want[0] || reapplier.gotKeys[1] != want[1] {
		t.Errorf("re-apply keys = %v, want %v (managed excluded, overlay applied)", reapplier.gotKeys, want)
	}
}

func TestSecretsSet_RejectsInvalidName(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := newSecretsSvc(fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{}, &fakeReapplier{}, &fakeDeploymentStore{})
	if _, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "1BAD-NAME", Value: "x"}}); !errors.Is(err, ErrInvalidSecretName) {
		t.Fatalf("want ErrInvalidSecretName, got %v", err)
	}
	if setter.env != "" {
		t.Error("an invalid name must be rejected before any GitHub write")
	}
}

func TestSecretsSet_RejectsManagedName(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := newSecretsSvc(fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{}, &fakeReapplier{}, &fakeDeploymentStore{})
	if _, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "SERVER_SSH_KEY", Value: "x"}}); !errors.Is(err, ErrManagedSecret) {
		t.Fatalf("want ErrManagedSecret, got %v", err)
	}
	if setter.env != "" {
		t.Error("a managed name must be rejected before any GitHub write")
	}
}

func TestSecretsSet_RejectsMultilineValue(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := newSecretsSvc(fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{}, &fakeReapplier{}, &fakeDeploymentStore{})
	if _, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "PEM", Value: "line1\nline2"}}); !errors.Is(err, ErrMultilineValue) {
		t.Fatalf("want ErrMultilineValue, got %v", err)
	}
	if setter.env != "" {
		t.Error("a multiline value must be rejected before any GitHub write")
	}
}

func TestSecretsSet_LegacyDeploymentReportsNotRedeployed(t *testing.T) {
	reapplier := &fakeReapplier{err: ErrLegacyDeployment}
	svc := newSecretsSvc(fakeSecretLister{}, &fakeSecretSetter{}, &fakeSecretDeleter{}, fakeExampleReader{}, reapplier, &fakeDeploymentStore{})
	result, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "API_KEY", Value: "sk-1"}})
	if err != nil {
		t.Fatalf("a legacy deployment must not fail the write: %v", err)
	}
	if result.Redeployed || result.Message == "" {
		t.Errorf("want redeployed=false with a message for a legacy deployment, got %+v", result)
	}
}

func TestSecretsSet_ReapplyErrorReportsNotRedeployed(t *testing.T) {
	reapplier := &fakeReapplier{err: errors.New("dispatch failed")}
	svc := newSecretsSvc(fakeSecretLister{}, &fakeSecretSetter{}, &fakeSecretDeleter{}, fakeExampleReader{}, reapplier, &fakeDeploymentStore{})
	result, err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "API_KEY", Value: "sk-1"}})
	if err != nil {
		t.Fatalf("a re-apply failure must not fail the write: %v", err)
	}
	if result.Redeployed || result.Message == "" {
		t.Errorf("want redeployed=false with a message on a re-apply error, got %+v", result)
	}
}

func TestSecretsDelete_RejectsManagedAndDeletesGitHub(t *testing.T) {
	deleter := &fakeSecretDeleter{}
	reapplier := &fakeReapplier{}
	// After deleting DATABASE_URL, the overlay excludes it even though the list
	// still reports it (GitHub lag).
	lister := fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}, {Name: "API_KEY"}}}
	svc := newSecretsSvc(lister, &fakeSecretSetter{}, deleter, fakeExampleReader{}, reapplier, &fakeDeploymentStore{})

	if _, err := svc.Delete(context.Background(), "acme", "shop", "production", "DEPLOY_DIR"); !errors.Is(err, ErrManagedSecret) {
		t.Fatalf("want ErrManagedSecret for managed name, got %v", err)
	}
	if len(deleter.deleted) != 0 {
		t.Error("a managed name must be rejected before any GitHub delete")
	}

	result, err := svc.Delete(context.Background(), "acme", "shop", "production", "DATABASE_URL")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(deleter.deleted) != 1 || deleter.deleted[0] != "DATABASE_URL" {
		t.Errorf("GitHub delete not invoked as expected: %+v", deleter.deleted)
	}
	if !result.Redeployed {
		t.Errorf("want redeployed after delete, got %+v", result)
	}
	// The deleted key must be excluded from the re-apply set even though the list lags.
	for _, k := range reapplier.gotKeys {
		if k == "DATABASE_URL" {
			t.Errorf("deleted key should be excluded from re-apply, got %v", reapplier.gotKeys)
		}
	}
	if len(reapplier.gotKeys) != 1 || reapplier.gotKeys[0] != "API_KEY" {
		t.Errorf("re-apply keys = %v, want [API_KEY]", reapplier.gotKeys)
	}
}
