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

func TestSecretsList_ClassifiesNamesAndUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lister := fakeSecretLister{infos: []SecretInfo{
		{Name: "SERVER_HOST", UpdatedAt: updated},  // managed
		{Name: "DATABASE_URL", UpdatedAt: updated}, // operator
	}}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}},
		&fakeDeploymentStore{},
		lister, &fakeSecretSetter{}, &fakeSecretDeleter{}, fakeExampleReader{},
	)

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
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, deployments,
		fakeSecretLister{infos: []SecretInfo{{Name: "DATABASE_URL"}}},
		&fakeSecretSetter{}, &fakeSecretDeleter{},
		fakeExampleReader{keys: []string{"DATABASE_URL", "REDIS_URL", "STRIPE_KEY"}},
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

func TestSecretsSet_WritesGitHub(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{},
	)
	if err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "DATABASE_URL", Value: "pg://new"}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if setter.env != "production" || len(setter.secrets) != 1 || setter.secrets[0].Value != "pg://new" {
		t.Errorf("GitHub write wrong: %+v", setter)
	}
}

func TestSecretsSet_RejectsInvalidName(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{},
	)
	if err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "1BAD-NAME", Value: "x"}}); !errors.Is(err, ErrInvalidSecretName) {
		t.Fatalf("want ErrInvalidSecretName, got %v", err)
	}
	if setter.env != "" {
		t.Error("an invalid name must be rejected before any GitHub write")
	}
}

func TestSecretsSet_RejectsManagedName(t *testing.T) {
	setter := &fakeSecretSetter{}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, setter, &fakeSecretDeleter{}, fakeExampleReader{},
	)
	if err := svc.Set(context.Background(), "acme", "shop", "production", []NamedSecret{{Name: "SERVER_SSH_KEY", Value: "x"}}); !errors.Is(err, ErrManagedSecret) {
		t.Fatalf("want ErrManagedSecret, got %v", err)
	}
	if setter.env != "" {
		t.Error("a managed name must be rejected before any GitHub write")
	}
}

func TestSecretsDelete_RejectsManagedAndDeletesGitHub(t *testing.T) {
	deleter := &fakeSecretDeleter{}
	svc := NewSecretsService(
		&fakeStore{loadTok: Token{AccessToken: "tok"}}, &fakeDeploymentStore{},
		fakeSecretLister{}, &fakeSecretSetter{}, deleter, fakeExampleReader{},
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
}
