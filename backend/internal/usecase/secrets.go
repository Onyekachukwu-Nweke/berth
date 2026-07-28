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

// SecretInfo is one environment secret as GitHub reports it: a name and when it
// was last updated. GitHub never returns the value.
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
}

// EnvSecretLister lists an environment's Actions secret names + updated times
// (never values), so mountabo can show what is configured and detect drift.
type EnvSecretLister interface {
	ListEnvSecrets(ctx context.Context, t Token, owner, repo, environment string) ([]SecretInfo, error)
}

// EnvSecretDeleter removes one Actions secret from a repo environment.
type EnvSecretDeleter interface {
	DeleteEnvSecret(ctx context.Context, t Token, owner, repo, environment, name string) error
}

// SecretValue is a locally-stored (revealable) secret value plus the GitHub
// updated-at mountabo observed when it last synced it, used to detect drift.
type SecretValue struct {
	Value               string
	GitHubUpdatedAtSeen time.Time
}

// SecretStore persists the revealable values mountabo set, per environment,
// encrypted at rest. GitHub remains the source of truth for which names are
// configured; this store is the source of truth for values mountabo created.
type SecretStore interface {
	SaveSecretValues(owner, repo, environment string, values map[string]SecretValue) error
	LoadSecretValues(owner, repo, environment string) (map[string]SecretValue, error)
	DeleteSecretValue(owner, repo, environment, name string) error
	ClearSecretValues(owner, repo, environment string) error
}

// Secret statuses surfaced to the UI.
const (
	SecretStale              = "stale"              // changed on GitHub since mountabo last synced it
	SecretSetOutsideMountabo = "setOutsideMountabo" // on GitHub, no local value
	SecretRemovedOnGitHub    = "removedOnGitHub"    // local value, name no longer on GitHub
)

// SecretView is one environment secret shaped for the UI.
type SecretView struct {
	Name          string    `json:"name"`
	Managed       bool      `json:"managed"`
	UpdatedAt     time.Time `json:"updatedAt"`
	HasLocalValue bool      `json:"hasLocalValue"`
	Value         string    `json:"value,omitempty"` // only when includeValues and locally known
	Status        string    `json:"status,omitempty"`
}

// SecretsView is the per-environment secrets tab payload.
type SecretsView struct {
	Secrets []SecretView `json:"secrets"`
	Missing []string     `json:"missing"` // .env.example keys not configured
}

// SetResult reports whether a Set fully synced. Partial is true when the secret
// was set on GitHub but its value could not be stored locally (so it won't be
// revealable) — the caller surfaces it as a warning, not a failure.
type SetResult struct {
	Partial bool   `json:"partial"`
	Message string `json:"message,omitempty"`
}

// SecretsService reads and manages an environment's Actions secrets on the
// connected user's behalf: GitHub is authoritative for which names are
// configured (and their updated times); the SecretStore holds the revealable
// values mountabo set. It reads a deployment's branch + root dir to diff against
// the repo's .env.example.
type SecretsService struct {
	tokens      TokenStore
	deployments DeploymentStore
	lister      EnvSecretLister
	setter      EnvSecretSetter
	deleter     EnvSecretDeleter
	store       SecretStore
	example     EnvExampleReader
}

// NewSecretsService wires the service to its ports.
func NewSecretsService(tokens TokenStore, deployments DeploymentStore, lister EnvSecretLister, setter EnvSecretSetter, deleter EnvSecretDeleter, store SecretStore, example EnvExampleReader) *SecretsService {
	return &SecretsService{tokens: tokens, deployments: deployments, lister: lister, setter: setter, deleter: deleter, store: store, example: example}
}

// List returns the environment's configured secrets (from GitHub, authoritative)
// merged with mountabo's local values, each classified managed/operator and
// flagged stale / set-outside / removed, plus the .env.example variables that
// are not yet configured. Values are included only when includeValues is true.
func (s *SecretsService) List(ctx context.Context, owner, repo, environment string, includeValues bool) (SecretsView, error) {
	token, err := s.tokens.Load()
	if err != nil {
		return SecretsView{}, fmt.Errorf("load token: %w", err)
	}
	infos, err := s.lister.ListEnvSecrets(ctx, token, owner, repo, environment)
	if err != nil {
		return SecretsView{}, fmt.Errorf("list environment secrets: %w", err)
	}
	// Local values are best-effort: a store read failure still shows GitHub's
	// authoritative names, just without revealable values.
	local, _ := s.store.LoadSecretValues(owner, repo, environment)

	onGitHub := make(map[string]bool, len(infos))
	views := make([]SecretView, 0, len(infos)+len(local))
	for _, info := range infos {
		onGitHub[info.Name] = true
		v := SecretView{Name: info.Name, Managed: isManagedSecret(info.Name), UpdatedAt: info.UpdatedAt}
		if lv, ok := local[info.Name]; ok {
			v.HasLocalValue = true
			if includeValues {
				v.Value = lv.Value
			}
			if !info.UpdatedAt.Equal(lv.GitHubUpdatedAtSeen) {
				v.Status = SecretStale
			}
		} else if !v.Managed {
			// An operator secret configured on GitHub that mountabo didn't set (or
			// whose value it didn't keep) — no revealable value here.
			v.Status = SecretSetOutsideMountabo
		}
		views = append(views, v)
	}
	// Local values whose name is no longer on GitHub: surface the drift.
	for name, lv := range local {
		if onGitHub[name] {
			continue
		}
		v := SecretView{Name: name, Managed: isManagedSecret(name), HasLocalValue: true, Status: SecretRemovedOnGitHub}
		if includeValues {
			v.Value = lv.Value
		}
		views = append(views, v)
	}

	return SecretsView{Secrets: views, Missing: s.missingKeys(ctx, token, owner, repo, environment, onGitHub)}, nil
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

// Set writes operator secrets to the GitHub environment, then records their
// values locally so they stay revealable. Managed names are rejected. A GitHub
// write failure is a hard error; a local-store failure after a successful GitHub
// write is a partial success (the secret is configured but not revealable).
func (s *SecretsService) Set(ctx context.Context, owner, repo, environment string, secrets []NamedSecret) (SetResult, error) {
	for _, sec := range secrets {
		if isManagedSecret(sec.Name) {
			return SetResult{}, ErrManagedSecret
		}
	}
	token, err := s.tokens.Load()
	if err != nil {
		return SetResult{}, fmt.Errorf("load token: %w", err)
	}
	if err := s.setter.SetEnvSecrets(ctx, token, owner, repo, environment, secrets); err != nil {
		return SetResult{}, fmt.Errorf("set environment secrets: %w", err)
	}
	values := make(map[string]string, len(secrets))
	for _, sec := range secrets {
		values[sec.Name] = sec.Value
	}
	if err := s.RecordSet(ctx, token, owner, repo, environment, values); err != nil {
		// Not an error to the caller: the source-of-truth GitHub write succeeded.
		// The local-store failure is surfaced as a partial result so the UI warns
		// the value won't be revealable, rather than failing a real configuration.
		return SetResult{ //nolint:nilerr // partial success is reported via SetResult.Partial, not an error
			Partial: true,
			Message: "set on GitHub, but the value couldn't be saved locally — it won't be revealable here; retry to store it",
		}, nil
	}
	return SetResult{}, nil
}

// RecordSet records already-set secret values locally, re-listing the
// environment to capture each name's GitHub updated-at (so drift is measured
// GitHub-clock to GitHub-clock, not against the local clock). Used by Set and by
// the deploy flow after it sets the operator's env vars.
func (s *SecretsService) RecordSet(ctx context.Context, token Token, owner, repo, environment string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	infos, err := s.lister.ListEnvSecrets(ctx, token, owner, repo, environment)
	if err != nil {
		return fmt.Errorf("re-list environment secrets: %w", err)
	}
	seen := make(map[string]time.Time, len(infos))
	for _, info := range infos {
		seen[info.Name] = info.UpdatedAt
	}
	record := make(map[string]SecretValue, len(values))
	for name, value := range values {
		record[name] = SecretValue{Value: value, GitHubUpdatedAtSeen: seen[name]}
	}
	return s.store.SaveSecretValues(owner, repo, environment, record)
}

// Delete removes an operator secret from the GitHub environment and clears its
// local value. Managed names are rejected with ErrManagedSecret.
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
	return s.store.DeleteSecretValue(owner, repo, environment, name)
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
