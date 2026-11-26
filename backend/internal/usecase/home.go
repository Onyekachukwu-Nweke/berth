package usecase

import (
	"context"
	"fmt"
	"strings"
)

// HomeAccount names the GitHub account that owns the Mountabo App: the agency's
// user or organization. The App is public so outside collaborators can sign in
// through it, which also lets anyone install it on their own account. Only
// repos owned by the home account count, so a stranger who installs the App on
// their own repos is still refused.
type HomeAccount interface {
	Owner(ctx context.Context) (string, error)
}

// homeRepos keeps the repos owned by the home account. With no home account
// configured it keeps everything. It fails closed: if the owner can't be read,
// nothing is kept.
func homeRepos[T any](ctx context.Context, h HomeAccount, repos []T, owner func(T) string) ([]T, error) {
	if h == nil {
		return repos, nil
	}
	home, err := h.Owner(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the App's owner: %w", err)
	}
	out := make([]T, 0, len(repos))
	for _, r := range repos {
		if strings.EqualFold(owner(r), home) {
			out = append(out, r)
		}
	}
	return out, nil
}

func accessOwner(r RepoAccess) string { return r.Owner }
func repoOwner(r Repo) string         { return r.Owner }
