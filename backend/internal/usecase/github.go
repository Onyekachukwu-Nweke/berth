// Package usecase holds berth's application logic and the ports it depends
// on. It owns the interfaces it consumes; adapters in internal/github and
// internal/vault satisfy them. No HTTP, OAuth, or GitHub library types leak
// across this boundary, only the domain types defined here.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// repoCacheTTL bounds how stale the cached repository list may be. Listing all
// of an account's repos is the slowest call in the app (full pagination against
// GitHub), so a short cache keeps repeat page loads instant while still picking
// up new repos within a minute.
const repoCacheTTL = 60 * time.Second

// ErrNotConnected is returned when the GitHub App is not installed on the
// account that owns a repo, so berth has no credential to act there.
var ErrNotConnected = errors.New("github app not installed for this account")

// Token is a GitHub credential: an App installation token for repo operations,
// or a signed-in user's token. User tokens from a GitHub App expire (Expiry)
// and are renewed with RefreshToken; installation tokens are minted on demand.
type Token struct {
	AccessToken  string
	RefreshToken string    `json:",omitempty"`
	Expiry       time.Time `json:",omitzero"`
}

// Repo is a GitHub repository the connected account can deploy from.
type Repo struct {
	Owner         string
	Name          string
	FullName      string
	Private       bool
	DefaultBranch string
	Language      string
	PushedAt      time.Time
	// HasDocker is true when the repo's root holds a Dockerfile or a Compose
	// file, so the UI can flag repos that are ready to containerize.
	HasDocker bool
	// Permission is the signed-in user's permission on the repo (from GitHub).
	Permission Permission
	// Kind is how the repo containerizes, from its root: "compose" (a Compose
	// file, takes precedence), "docker" (a Dockerfile only), or "none". Lets the
	// UI filter compose / docker / non-docker repos.
	Kind string
}

// RepoRef points at a place inside a repository to read container config from:
// owner/name at an optional ref (branch or sha; empty means the default branch)
// and an optional sub-directory (empty means the repo root) where the compose
// file or Dockerfile lives.
type RepoRef struct {
	Owner string
	Name  string
	Ref   string
	Dir   string
}

// ServicePort is one published port berth detected in a repo's container
// configuration. When EnvVar is set, the compose file binds the host port to
// that environment variable (e.g. "${FRONTEND_PORT:-3000}:3000"), so berth
// can choose the value and inject it at deploy time; Editable is true and Host
// carries the default. When EnvVar is empty the host port is a literal in the
// file (or a Dockerfile EXPOSE line) that berth cannot remap, so it is shown
// read-only.
type ServicePort struct {
	Service   string // compose service name; empty for a Dockerfile EXPOSE port
	EnvVar    string // host-port env var, empty when the host port is a literal
	Host      string // host port value or env-var default; empty if auto-assigned
	Container string // container port
	Editable  bool   // true when berth can set the host port via EnvVar
}

// RepoTokens mints the credential for acting on an owner's repositories: the
// GitHub App's installation token for that account. It returns ErrNotConnected
// when the App is not installed there.
type RepoTokens interface {
	TokenFor(ctx context.Context, owner string) (Token, error)
}

// UserTokens returns a signed-in user's own GitHub token, renewed if needed.
type UserTokens interface {
	UserToken(ctx context.Context, user User) (Token, error)
}

// RepoLister lists every repository a token can access, public and private,
// owned, collaborator, and organization.
type RepoLister interface {
	List(ctx context.Context, t Token) ([]Repo, error)
}

// DeployStrategy is how a repo is deployed: a docker compose stack, or a single
// container built from its Dockerfile. Empty when neither is detected.
type DeployStrategy string

// Deploy strategies, matching the values the workflow generator understands.
const (
	StrategyCompose DeployStrategy = "compose"
	StrategyDocker  DeployStrategy = "docker"
)

// PortDetector reads a repo's container configuration (a compose file, else a
// Dockerfile) at the given ref and reports the published ports it declares plus
// the deploy strategy that fits. A repo with no detectable ports returns an
// empty slice and an empty strategy, not an error.
type PortDetector interface {
	DetectPorts(ctx context.Context, t Token, ref RepoRef) ([]ServicePort, DeployStrategy, error)
}

// GitHubConnector reads repositories on a user's behalf: which ones they can
// deploy, and the container config inside them.
type GitHubConnector struct {
	repos  RepoLister
	ports  PortDetector
	tokens RepoTokens
	users  UserTokens
	gate   RepoGate

	mu        sync.Mutex // guards the per-user repo cache below
	repoCache map[int64]cachedRepos
}

type cachedRepos struct {
	repos []Repo
	at    time.Time
}

// NewGitHubConnector wires the connector to its ports. gate limits developers'
// repo picker to the deploy sources; nil keeps every repo.
func NewGitHubConnector(repos RepoLister, ports PortDetector, tokens RepoTokens, users UserTokens, gate RepoGate) *GitHubConnector {
	return &GitHubConnector{repos: repos, ports: ports, tokens: tokens, users: users, gate: gate, repoCache: map[int64]cachedRepos{}}
}

// Repositories lists the repos user can reach through the App, with their
// permission on each, read with the user's own token so GitHub decides.
func (c *GitHubConnector) Repositories(ctx context.Context, user User) ([]Repo, error) {
	c.mu.Lock()
	if hit, ok := c.repoCache[user.ID]; ok && time.Since(hit.at) < repoCacheTTL {
		c.mu.Unlock()
		return c.forUser(ctx, user, hit.repos)
	}
	c.mu.Unlock()

	token, err := c.users.UserToken(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("load user token: %w", err)
	}
	repos, err := c.repos.List(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}

	c.mu.Lock()
	c.repoCache[user.ID] = cachedRepos{repos: repos, at: time.Now()}
	c.mu.Unlock()
	return c.forUser(ctx, user, repos)
}

// forUser limits a developer's repos to the deploy sources. The cache holds
// GitHub's full answer, so a change to the sources shows at once.
func (c *GitHubConnector) forUser(ctx context.Context, user User, repos []Repo) ([]Repo, error) {
	if user.IsAdmin || c.gate == nil {
		return repos, nil
	}
	allow, err := c.gate.DeployGate(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(repos))
	for _, r := range repos {
		if allow(r.Owner, r.Name) {
			out = append(out, r)
		}
	}
	return out, nil
}

// DetectPorts reads the published ports declared in a repo's container config,
// so the UI can offer the project's real ports instead of a fixed guess. It
// returns an empty slice (no error) when the repo declares no detectable ports.
func (c *GitHubConnector) DetectPorts(ctx context.Context, ref RepoRef) ([]ServicePort, DeployStrategy, error) {
	token, err := c.tokens.TokenFor(ctx, ref.Owner)
	if err != nil {
		return nil, "", fmt.Errorf("installation token: %w", err)
	}
	ports, strategy, err := c.ports.DetectPorts(ctx, token, ref)
	if err != nil {
		return nil, "", fmt.Errorf("detect ports: %w", err)
	}
	return ports, strategy, nil
}
