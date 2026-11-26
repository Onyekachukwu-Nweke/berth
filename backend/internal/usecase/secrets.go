package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrManagedSecret is returned when the UI tries to edit or delete a
// mountabo-managed connection secret (SERVER_*, DEPLOY_DIR).
var ErrManagedSecret = errors.New("managed secret cannot be edited or deleted")

// ErrInvalidSecretName is returned when a secret name isn't a valid GitHub
// Actions secret name (letters, digits, underscores; not starting with a digit).
var ErrInvalidSecretName = errors.New("invalid secret name")

// ErrMultilineValue is returned when a secret value contains a newline. Values
// are injected as single .env lines (docker --env-file / compose .env), which
// can't represent multiline values, so berth rejects them up front. (The
// managed SERVER_SSH_KEY may be multiline but is set via the deploy path and is
// never written to .env, so it is unaffected.)
var ErrMultilineValue = errors.New("secret value cannot contain newlines")

// DeployReapplier regenerates a deployment's committed workflow + deploy.sh for
// the given operator env-var names and triggers a redeploy, so a secret change
// reaches the running container. Implemented by *DeployService.
type DeployReapplier interface {
	ReapplyWorkflow(ctx context.Context, t Token, owner, repo, environment string, envKeys []string) error
}

// ApplyResult reports whether a secret change actually triggered a redeploy. When
// Redeployed is false, Message explains why (e.g. a legacy deployment that must
// be redeployed from configure, or a dispatch that couldn't be triggered) — the
// value is on GitHub either way, but the running container hasn't picked it up.
type ApplyResult struct {
	Redeployed bool   `json:"redeployed"`
	Message    string `json:"message,omitempty"`
}

// SecretInfo is one environment secret as GitHub reports it: a name and when it
// was last updated. GitHub never returns the value.
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
}

// EnvSecretLister lists an environment's Actions secret names + updated times
// (never values), so berth can show what is configured.
type EnvSecretLister interface {
	ListEnvSecrets(ctx context.Context, t Token, owner, repo, environment string) ([]SecretInfo, error)
}

// EnvSecretDeleter removes one Actions secret from a repo environment.
type EnvSecretDeleter interface {
	DeleteEnvSecret(ctx context.Context, t Token, owner, repo, environment, name string) error
}

// SecretView is one environment secret shaped for the UI. GitHub Actions secret
// values are write-only, so there is no value field: berth shows the name, the
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
// connected user's behalf. GitHub is the single source of truth: berth can
// list the configured names (+ updated times), set values (write-only), and
// delete secrets, but never reads a value back. It reads a deployment's branch +
// root dir to diff the configured names against the repo's .env.example.
type SecretsService struct {
	tokens      RepoTokens
	deployments DeploymentStore
	lister      EnvSecretLister
	setter      EnvSecretSetter
	deleter     EnvSecretDeleter
	example     EnvExampleReader
	reapplier   DeployReapplier
}

// NewSecretsService wires the service to its ports.
func NewSecretsService(tokens RepoTokens, deployments DeploymentStore, lister EnvSecretLister, setter EnvSecretSetter, deleter EnvSecretDeleter, example EnvExampleReader, reapplier DeployReapplier) *SecretsService {
	return &SecretsService{tokens: tokens, deployments: deployments, lister: lister, setter: setter, deleter: deleter, example: example, reapplier: reapplier}
}

// List returns the environment's configured secrets from GitHub (name, managed
// classification, and updated time), plus the .env.example variable names that
// are not yet configured. Values are never returned — GitHub secrets are
// write-only.
func (s *SecretsService) List(ctx context.Context, owner, repo, environment string) (SecretsView, error) {
	token, err := s.tokens.TokenFor(ctx, owner)
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
// GitHub is the only store), then re-applies the deployment so the running
// container picks them up. Managed names, invalid names, and multiline values
// are rejected before any write. A GitHub write failure is a hard error; a
// re-apply failure is not — the value is on GitHub, but the returned ApplyResult
// reports Redeployed=false so the caller can warn "redeploy to apply".
func (s *SecretsService) Set(ctx context.Context, owner, repo, environment string, secrets []NamedSecret) (ApplyResult, error) {
	for _, sec := range secrets {
		if isManagedSecret(sec.Name) {
			return ApplyResult{}, ErrManagedSecret
		}
		if !secretName.MatchString(sec.Name) {
			return ApplyResult{}, fmt.Errorf("%w: %q", ErrInvalidSecretName, sec.Name)
		}
		if strings.ContainsAny(sec.Value, "\r\n") {
			return ApplyResult{}, fmt.Errorf("%w: %q", ErrMultilineValue, sec.Name)
		}
	}
	token, err := s.tokens.TokenFor(ctx, owner)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("load token: %w", err)
	}
	if err := s.setter.SetEnvSecrets(ctx, token, owner, repo, environment, secrets); err != nil {
		return ApplyResult{}, fmt.Errorf("set environment secrets: %w", err)
	}
	added := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		added = append(added, sec.Name)
	}
	return s.reapply(ctx, token, owner, repo, environment, added, ""), nil
}

// Delete removes an operator secret from the GitHub environment, then re-applies
// the deployment so it stops reaching the container. Managed names are rejected.
func (s *SecretsService) Delete(ctx context.Context, owner, repo, environment, name string) (ApplyResult, error) {
	if isManagedSecret(name) {
		return ApplyResult{}, ErrManagedSecret
	}
	token, err := s.tokens.TokenFor(ctx, owner)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("load token: %w", err)
	}
	if err := s.deleter.DeleteEnvSecret(ctx, token, owner, repo, environment, name); err != nil {
		return ApplyResult{}, fmt.Errorf("delete environment secret: %w", err)
	}
	return s.reapply(ctx, token, owner, repo, environment, nil, name), nil
}

// reapply regenerates the deployment's artifacts for the current operator secret
// names and triggers a redeploy, mapping the outcome to an ApplyResult. The
// GitHub write already succeeded, so a re-apply failure only downgrades to
// Redeployed=false with a message rather than surfacing as an error.
func (s *SecretsService) reapply(ctx context.Context, token Token, owner, repo, environment string, add []string, remove string) ApplyResult {
	keys := s.desiredKeys(ctx, token, owner, repo, environment, add, remove)
	err := s.reapplier.ReapplyWorkflow(ctx, token, owner, repo, environment, keys)
	switch {
	case err == nil:
		return ApplyResult{Redeployed: true}
	case errors.Is(err, ErrLegacyDeployment):
		return ApplyResult{Message: "Saved on GitHub. This deployment predates secrets-tab apply — run a full redeploy from the configure page to apply changes."}
	default:
		return ApplyResult{Message: "Saved on GitHub, but the redeploy couldn't be triggered — redeploy to apply."}
	}
}

// desiredKeys is the operator secret names the workflow should forward after a
// change: the live GitHub list (managed names excluded) overlaid with the just-
// applied mutation (added names in, the deleted name out), so the changed key is
// always correct even if GitHub's list momentarily lags the write. The list is
// retried briefly on error. Sorted for a stable, diff-friendly workflow.
func (s *SecretsService) desiredKeys(ctx context.Context, token Token, owner, repo, environment string, add []string, remove string) []string {
	var infos []SecretInfo
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		if infos, err = s.lister.ListEnvSecrets(ctx, token, owner, repo, environment); err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	names := make(map[string]bool, len(infos)+len(add))
	for _, info := range infos {
		if !isManagedSecret(info.Name) {
			names[info.Name] = true
		}
	}
	for _, a := range add {
		if a != "" && !isManagedSecret(a) {
			names[a] = true
		}
	}
	delete(names, remove)
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
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
