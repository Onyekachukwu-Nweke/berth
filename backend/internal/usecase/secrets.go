package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrManagedSecret is returned when the UI tries to edit or delete a
// mountabo-managed connection secret (SERVER_*, DEPLOY_DIR).
var ErrManagedSecret = errors.New("managed secret cannot be edited or deleted")

// ErrInvalidSecretName is returned when a secret name isn't a valid GitHub
// Actions secret name (letters, digits, underscores; not starting with a digit).
var ErrInvalidSecretName = errors.New("invalid secret name")

// SecretInfo is one environment secret as GitHub reports it: a name and when it
// was last updated. GitHub never returns the value.
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
}

// EnvSecretLister lists an environment's Actions secret names + updated times
// (never values), so mountabo can show what is configured.
type EnvSecretLister interface {
	ListEnvSecrets(ctx context.Context, t Token, owner, repo, environment string) ([]SecretInfo, error)
}

// EnvSecretDeleter removes one Actions secret from a repo environment.
type EnvSecretDeleter interface {
	DeleteEnvSecret(ctx context.Context, t Token, owner, repo, environment, name string) error
}

// SecretView is one environment secret shaped for the UI. GitHub Actions secret
// values are write-only, so there is no value field: mountabo shows the name, the
// managed classification, and GitHub's last-updated time only.
type SecretView struct {
	Name      string    `json:"name"`
	Managed   bool      `json:"managed"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SecretsView is the per-environment secrets tab payload.
type SecretsView struct {
	Secrets []SecretView `json:"secrets"`
	Missing []string     `json:"missing"` // .env.example keys not configured
}

// SecretsService reads and manages an environment's GitHub Actions secrets on the
// connected user's behalf. GitHub is the single source of truth: mountabo can
// list the configured names (+ updated times), set values (write-only), and
// delete secrets, but never reads a value back. It reads a deployment's branch +
// root dir to diff the configured names against the repo's .env.example.
type SecretsService struct {
	tokens      TokenStore
	deployments DeploymentStore
	lister      EnvSecretLister
	setter      EnvSecretSetter
	deleter     EnvSecretDeleter
	example     EnvExampleReader
}

// NewSecretsService wires the service to its ports.
func NewSecretsService(tokens TokenStore, deployments DeploymentStore, lister EnvSecretLister, setter EnvSecretSetter, deleter EnvSecretDeleter, example EnvExampleReader) *SecretsService {
	return &SecretsService{tokens: tokens, deployments: deployments, lister: lister, setter: setter, deleter: deleter, example: example}
}

// List returns the environment's configured secrets from GitHub (name, managed
// classification, and updated time), plus the .env.example variable names that
// are not yet configured. Values are never returned — GitHub secrets are
// write-only.
func (s *SecretsService) List(ctx context.Context, owner, repo, environment string) (SecretsView, error) {
	token, err := s.tokens.Load()
	if err != nil {
		return SecretsView{}, fmt.Errorf("load token: %w", err)
	}
	infos, err := s.lister.ListEnvSecrets(ctx, token, owner, repo, environment)
	if err != nil {
		return SecretsView{}, fmt.Errorf("list environment secrets: %w", err)
	}

	configured := make(map[string]bool, len(infos))
	views := make([]SecretView, 0, len(infos))
	for _, info := range infos {
		configured[info.Name] = true
		views = append(views, SecretView{
			Name:      info.Name,
			Managed:   isManagedSecret(info.Name),
			UpdatedAt: info.UpdatedAt,
		})
	}

	return SecretsView{Secrets: views, Missing: s.missingKeys(ctx, token, owner, repo, environment, configured)}, nil
}

// missingKeys returns the repo's .env.example variable names that are not among
// the configured secret names, using the deployment's branch + root dir. Empty
// (not an error) when there is no tracked deployment or no example file.
func (s *SecretsService) missingKeys(ctx context.Context, token Token, owner, repo, environment string, configured map[string]bool) []string {
	dep, found := s.findDeployment(owner, repo, environment)
	if !found {
		return []string{}
	}
	keys, err := s.example.EnvExampleKeys(ctx, token, RepoRef{Owner: owner, Name: repo, Ref: dep.Branch, Dir: dep.RootDir})
	if err != nil {
		return []string{}
	}
	missing := make([]string, 0)
	for _, k := range keys {
		if !configured[k] {
			missing = append(missing, k)
		}
	}
	return missing
}

// Set writes operator secrets to the GitHub environment (values are write-only:
// GitHub is the only store). Managed names and names that aren't valid GitHub
// Actions secret names are rejected before any write.
func (s *SecretsService) Set(ctx context.Context, owner, repo, environment string, secrets []NamedSecret) error {
	for _, sec := range secrets {
		if isManagedSecret(sec.Name) {
			return ErrManagedSecret
		}
		if !secretName.MatchString(sec.Name) {
			return fmt.Errorf("%w: %q", ErrInvalidSecretName, sec.Name)
		}
	}
	token, err := s.tokens.Load()
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}
	if err := s.setter.SetEnvSecrets(ctx, token, owner, repo, environment, secrets); err != nil {
		return fmt.Errorf("set environment secrets: %w", err)
	}
	return nil
}

// Delete removes an operator secret from the GitHub environment. Managed names
// are rejected with ErrManagedSecret.
func (s *SecretsService) Delete(ctx context.Context, owner, repo, environment, name string) error {
	if isManagedSecret(name) {
		return ErrManagedSecret
	}
	token, err := s.tokens.Load()
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}
	if err := s.deleter.DeleteEnvSecret(ctx, token, owner, repo, environment, name); err != nil {
		return fmt.Errorf("delete environment secret: %w", err)
	}
	return nil
}

// findDeployment returns the tracked deployment for (owner, repo, environment).
func (s *SecretsService) findDeployment(owner, repo, environment string) (Deployment, bool) {
	deployments, err := s.deployments.List()
	if err != nil {
		return Deployment{}, false
	}
	for _, d := range deployments {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			return d, true
		}
	}
	return Deployment{}, false
}
