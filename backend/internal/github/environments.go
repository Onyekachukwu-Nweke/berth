package github

import (
	"context"
	"fmt"

	"github.com/goodylili/mountabo/internal/usecase"
	gogithub "github.com/google/go-github/v88/github"
)

var (
	_ usecase.EnvManager             = (*Client)(nil)
	_ usecase.EnvSecretSetter        = (*Client)(nil)
	_ usecase.EnvSecretLister        = (*Client)(nil)
	_ usecase.EnvSecretDeleter       = (*Client)(nil)
	_ usecase.RepoEnvironmentDeleter = (*Client)(nil)
	_ usecase.WorkflowDispatcher     = (*Client)(nil)
)

// Dispatch triggers a workflow run via the workflow_dispatch event, by workflow
// file name on the given ref (branch). Used to redeploy after a secret change so
// the running container picks up the new value; the workflow must declare
// `workflow_dispatch:` (mountabo's generated workflows do).
func (c *Client) Dispatch(ctx context.Context, t usecase.Token, owner, repo, workflowFile, ref string) error {
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}
	if _, _, err := api.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, workflowFile,
		gogithub.CreateWorkflowDispatchEventRequest{Ref: ref}); err != nil {
		return fmt.Errorf("dispatch workflow %q on %s/%s@%s: %w", workflowFile, owner, repo, ref, err)
	}
	return nil
}

// DeleteEnvironment removes a deployment environment (and the secrets scoped to
// it, GitHub drops them together) from owner/repo. A 404 means the environment
// is already gone, which is a no-op success: the teardown wants the end state
// "no such environment", not a hard failure when it never existed.
func (c *Client) DeleteEnvironment(ctx context.Context, t usecase.Token, owner, repo, name string) error {
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}
	resp, err := api.Repositories.DeleteEnvironment(ctx, owner, repo, name)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil
		}
		return fmt.Errorf("delete environment %q on %s/%s: %w", name, owner, repo, err)
	}
	return nil
}

// EnsureEnvironment creates or updates the named deployment environment on
// owner/repo. With reviewers (GitHub logins), every job that uses it waits
// until one of them approves, and repo admins can't bypass that. Without, the
// protection is cleared: mountabo owns the environment's rules.
func (c *Client) EnsureEnvironment(ctx context.Context, t usecase.Token, owner, repo, name string, reviewers []string) error {
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}
	env := &gogithub.CreateUpdateEnvironment{}
	if len(reviewers) > 0 {
		for _, login := range reviewers {
			u, _, err := api.Users.Get(ctx, login)
			if err != nil {
				return fmt.Errorf("look up approver %q: %w", login, err)
			}
			env.Reviewers = append(env.Reviewers, &gogithub.EnvReviewers{Type: gogithub.Ptr("User"), ID: u.ID})
		}
		env.CanAdminsBypass = gogithub.Ptr(false)
	}
	if _, _, err := api.Repositories.CreateUpdateEnvironment(ctx, owner, repo, name, env); err != nil {
		if len(reviewers) > 0 {
			return fmt.Errorf("require approval on environment %q of %s/%s (private repos need GitHub Team or Enterprise for required reviewers, and every approver needs access to the repo): %w", name, owner, repo, err)
		}
		return fmt.Errorf("create environment %q on %s/%s: %w", name, owner, repo, err)
	}
	return nil
}

// SetEnvSecrets stores secrets on owner/repo's environment. GitHub's env-secret
// API is keyed by repo id and requires each value sealed against the
// environment's public key, so it resolves the id and key once, then seals and
// uploads each secret. Values never appear in errors or logs.
func (c *Client) SetEnvSecrets(ctx context.Context, t usecase.Token, owner, repo, environment string, secrets []usecase.NamedSecret) error {
	if len(secrets) == 0 {
		return nil
	}
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}

	id, err := repoID(ctx, api, owner, repo)
	if err != nil {
		return err
	}

	pubKey, _, err := api.Actions.GetEnvPublicKey(ctx, id, environment)
	if err != nil {
		return fmt.Errorf("get public key for environment %q on %s/%s: %w", environment, owner, repo, err)
	}

	for _, secret := range secrets {
		encrypted, err := sealSecret(pubKey.GetKey(), secret.Value)
		if err != nil {
			return fmt.Errorf("encrypt secret %q: %w", secret.Name, err)
		}
		if _, err := api.Actions.CreateOrUpdateEnvSecret(ctx, id, environment, &gogithub.EncryptedSecret{
			Name:           secret.Name,
			KeyID:          pubKey.GetKeyID(),
			EncryptedValue: encrypted,
		}); err != nil {
			return fmt.Errorf("set secret %q on environment %q of %s/%s: %w", secret.Name, environment, owner, repo, err)
		}
	}
	return nil
}

// ListEnvSecrets returns the names + last-updated times of the Actions secrets on
// owner/repo's environment (never their values). Pages through every secret so
// the configured-name view is complete, not just the first page.
func (c *Client) ListEnvSecrets(ctx context.Context, t usecase.Token, owner, repo, environment string) ([]usecase.SecretInfo, error) {
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return nil, fmt.Errorf("build github client: %w", err)
	}
	repoID, err := repoID(ctx, api, owner, repo)
	if err != nil {
		return nil, err
	}

	opts := &gogithub.ListOptions{PerPage: 100}
	out := []usecase.SecretInfo{}
	for {
		secrets, resp, err := api.Actions.ListEnvSecrets(ctx, repoID, environment, opts)
		if err != nil {
			return nil, fmt.Errorf("list secrets for environment %q on %s/%s: %w", environment, owner, repo, err)
		}
		for _, s := range secrets.Secrets {
			out = append(out, usecase.SecretInfo{Name: s.GetName(), UpdatedAt: s.UpdatedAt.Time})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// DeleteEnvSecret removes one Actions secret from owner/repo's environment.
func (c *Client) DeleteEnvSecret(ctx context.Context, t usecase.Token, owner, repo, environment, name string) error {
	api, err := gogithub.NewClient(gogithub.WithAuthToken(t.AccessToken))
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}
	id, err := repoID(ctx, api, owner, repo)
	if err != nil {
		return err
	}
	if _, err := api.Actions.DeleteEnvSecret(ctx, id, environment, name); err != nil {
		return fmt.Errorf("delete secret %q on environment %q of %s/%s: %w", name, environment, owner, repo, err)
	}
	return nil
}

// repoID resolves owner/repo to the numeric id GitHub's env-secret API is keyed by.
func repoID(ctx context.Context, api *gogithub.Client, owner, repo string) (int, error) {
	repository, _, err := api.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("look up %s/%s: %w", owner, repo, err)
	}
	return int(repository.GetID()), nil
}
