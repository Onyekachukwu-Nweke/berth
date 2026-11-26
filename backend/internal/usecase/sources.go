package usecase

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Source is one place developers may deploy from: repos of Account whose name
// matches Pattern ("*" for all of them, or a wildcard such as "meals-*").
// Admins can deploy from any repo the App reaches; sources bind developers, so
// a developer can't bring in repos from their personal account.
type Source struct {
	Account string `json:"account"`
	Pattern string `json:"pattern"`
}

// Matches reports whether owner/repo falls under the source. Names compare
// case-insensitively, as GitHub's do.
func (s Source) Matches(owner, repo string) bool {
	if !strings.EqualFold(s.Account, owner) {
		return false
	}
	ok, err := path.Match(strings.ToLower(s.Pattern), strings.ToLower(repo))
	return err == nil && ok
}

// SourceWriter adds and removes deploy sources.
type SourceWriter interface {
	SaveSource(s Source) error
	DeleteSource(account, pattern string) error
}

// HomeAccount names the GitHub account that owns the Mountabo App: the agency's
// user or organization. With no sources configured, it is the only one.
type HomeAccount interface {
	Owner(ctx context.Context) (string, error)
}

// RepoGate decides which repos a developer may use. Sign-in, the repo picker
// and every access check go through it, so a rule change applies at once.
type RepoGate interface {
	DeployGate(ctx context.Context) (func(owner, repo string) bool, error)
}

var _ RepoGate = (*AccessService)(nil)

// DeployGate returns the predicate for developers' repos: the configured
// sources, or the App owner's whole account when there are none. It fails
// closed when the App owner can't be read.
func (a *AccessService) DeployGate(ctx context.Context) (func(owner, repo string) bool, error) {
	policy, err := a.policy.Policy()
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	sources := policy.Sources
	if len(sources) == 0 {
		if a.home == nil {
			return func(string, string) bool { return true }, nil
		}
		owner, err := a.home.Owner(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the App's owner: %w", err)
		}
		sources = []Source{{Account: owner, Pattern: "*"}}
	}
	return func(owner, repo string) bool {
		for _, s := range sources {
			if s.Matches(owner, repo) {
				return true
			}
		}
		return false
	}, nil
}

// gate is DeployGate for callers without a request context.
func (a *AccessService) gate() (func(owner, repo string) bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.DeployGate(ctx)
}

// HomeOwner is the account sources default to, for the admin page. Empty when
// it can't be read.
func (a *AccessService) HomeOwner(ctx context.Context) string {
	if a.home == nil {
		return ""
	}
	owner, err := a.home.Owner(ctx)
	if err != nil {
		return ""
	}
	return owner
}

// GitHub logins and org names: alphanumerics and single hyphens, up to 39.
var accountPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){0,38}$`)

// SaveSource adds a place developers may deploy from.
func (a *AccessService) SaveSource(s Source) error {
	s.Account = strings.ToLower(strings.TrimSpace(s.Account))
	s.Pattern = strings.ToLower(strings.TrimSpace(s.Pattern))
	if s.Pattern == "" {
		s.Pattern = "*"
	}
	if !accountPattern.MatchString(s.Account) {
		return forbidden("%q isn't a GitHub account name", s.Account)
	}
	if strings.Contains(s.Pattern, "/") {
		return forbidden("the pattern matches repo names only; put the account in its own field")
	}
	if _, err := path.Match(s.Pattern, ""); err != nil {
		return forbidden("%q isn't a valid pattern; use * as a wildcard, e.g. meals-*", s.Pattern)
	}
	return a.policy.SaveSource(s)
}

// DeleteSource removes a place developers may deploy from.
func (a *AccessService) DeleteSource(account, pattern string) error {
	return a.policy.DeleteSource(strings.ToLower(account), strings.ToLower(pattern))
}

// reachable is u's repo access as last synced, limited to the deploy sources
// for developers. Admins aren't limited.
func (a *AccessService) reachable(u User) ([]RepoAccess, error) {
	repos, err := a.repos.RepoAccessFor(u.ID)
	if err != nil {
		return nil, fmt.Errorf("read repo access: %w", err)
	}
	if u.IsAdmin {
		return repos, nil
	}
	allow, err := a.gate()
	if err != nil {
		return nil, err
	}
	out := make([]RepoAccess, 0, len(repos))
	for _, r := range repos {
		if allow(r.Owner, r.Repo) {
			out = append(out, r)
		}
	}
	return out, nil
}

// outsideSources reports whether GitHub gives u access to owner/repo but the
// deploy sources hide it, so a refusal can say which one stopped them.
func (a *AccessService) outsideSources(u User, owner, repo string) bool {
	if u.IsAdmin {
		return false
	}
	all, err := a.repos.RepoAccessFor(u.ID)
	if err != nil {
		return false
	}
	for _, r := range all {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, repo) && r.Permission >= PermRead {
			return true
		}
	}
	return false
}
