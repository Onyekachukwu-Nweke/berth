package usecase

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Access control has two sources of truth, kept deliberately separate:
//
//   - Which repos a person may touch is GitHub's answer. mountabo never keeps
//     its own copy of "who is on which project"; it asks GitHub (through the
//     App) and caches the answer per user for a few minutes.
//   - Where code may run is mountabo's policy: clients, the servers each one
//     owns, and per-client environment rules. GitHub knows nothing about
//     servers, so this is the one thing admins configure.

// Permission is a user's level on a repository, ordered so comparisons read
// naturally: PermWrite or better may deploy.
type Permission int

// Permission levels, lowest first.
const (
	PermNone Permission = iota
	PermRead
	PermWrite
	PermAdmin
)

// String is the wire and storage form of a permission.
func (p Permission) String() string {
	switch p {
	case PermRead:
		return "read"
	case PermWrite:
		return "write"
	case PermAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParsePermission reads the stored form back; unknown values are PermNone.
func ParsePermission(s string) Permission {
	switch s {
	case "read":
		return PermRead
	case "write":
		return PermWrite
	case "admin":
		return PermAdmin
	default:
		return PermNone
	}
}

// AgencyClientID is the built-in client every repo not linked to a real client
// resolves to: the agency's own projects.
const AgencyClientID = "agency"

// User is a person signed in to mountabo through GitHub.
type User struct {
	ID            int64     `json:"id"`
	GitHubID      int64     `json:"githubId"`
	Login         string    `json:"login"`
	Name          string    `json:"name"`
	AvatarURL     string    `json:"avatarUrl"`
	IsAdmin       bool      `json:"isAdmin"`
	ReposSyncedAt time.Time `json:"-"`
	LastLoginAt   time.Time `json:"lastLoginAt"`
}

// RepoAccess is one repo a user can reach and their permission on it.
type RepoAccess struct {
	Owner      string     `json:"owner"`
	Repo       string     `json:"repo"`
	Permission Permission `json:"-"`
}

// Client is a customer of the agency. Its repos and servers are isolated from
// every other client's.
type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// RepoLink assigns a repo to a client. Repo "*" assigns every repo of Owner.
type RepoLink struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	ClientID string `json:"clientId"`
}

// EnvRule says where one of a client's environments deploys.
type EnvRule struct {
	ClientID        string `json:"clientId"`
	Environment     string `json:"environment"`
	ServerID        string `json:"serverId"`
	RequireApproval bool   `json:"requireApproval"`
}

// RepoRule overrides a client's rule for one repo and environment.
type RepoRule struct {
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Environment     string `json:"environment"`
	ServerID        string `json:"serverId"`
	RequireApproval bool   `json:"requireApproval"`
}

// Policy is the whole placement policy. It is small (tens of rows), so it is
// read as one snapshot and resolved in memory.
type Policy struct {
	Clients   []Client
	Links     []RepoLink
	EnvRules  []EnvRule
	RepoRules []RepoRule
}

// ClientFor resolves the client a repo belongs to: an exact link, then an
// owner-wide link, then the agency.
func (p Policy) ClientFor(owner, repo string) string {
	owned := ""
	for _, l := range p.Links {
		if !strings.EqualFold(l.Owner, owner) {
			continue
		}
		if strings.EqualFold(l.Repo, repo) {
			return l.ClientID
		}
		if l.Repo == "*" {
			owned = l.ClientID
		}
	}
	if owned != "" {
		return owned
	}
	return AgencyClientID
}

// Target is where one environment of a repo deploys, and why.
type Target struct {
	Environment     string `json:"environment"`
	ServerID        string `json:"serverId"`
	RequireApproval bool   `json:"requireApproval"`
	// Source is "repo" for a repo override, "client" for the client's rule.
	Source string `json:"source"`
}

// TargetFor resolves where owner/repo's environment deploys: a repo override
// wins over the client's rule. ok is false when nothing is configured.
func (p Policy) TargetFor(owner, repo, environment string) (Target, bool) {
	for _, r := range p.RepoRules {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, repo) && strings.EqualFold(r.Environment, environment) {
			return Target{Environment: r.Environment, ServerID: r.ServerID, RequireApproval: r.RequireApproval, Source: "repo"}, true
		}
	}
	client := p.ClientFor(owner, repo)
	for _, r := range p.EnvRules {
		if r.ClientID == client && strings.EqualFold(r.Environment, environment) {
			return Target{Environment: r.Environment, ServerID: r.ServerID, RequireApproval: r.RequireApproval, Source: "client"}, true
		}
	}
	return Target{}, false
}

// TargetsFor lists every configured environment of owner/repo, repo overrides
// replacing the client's rule for the same environment.
func (p Policy) TargetsFor(owner, repo string) []Target {
	byEnv := map[string]Target{}
	var order []string
	add := func(t Target) {
		key := strings.ToLower(t.Environment)
		if _, seen := byEnv[key]; !seen {
			order = append(order, key)
		}
		byEnv[key] = t
	}
	client := p.ClientFor(owner, repo)
	for _, r := range p.EnvRules {
		if r.ClientID == client {
			add(Target{Environment: r.Environment, ServerID: r.ServerID, RequireApproval: r.RequireApproval, Source: "client"})
		}
	}
	for _, r := range p.RepoRules {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, repo) {
			add(Target{Environment: r.Environment, ServerID: r.ServerID, RequireApproval: r.RequireApproval, Source: "repo"})
		}
	}
	out := make([]Target, 0, len(order))
	for _, k := range order {
		out = append(out, byEnv[k])
	}
	return out
}

// ForbiddenError is a refusal with a reason the person can act on.
type ForbiddenError struct{ Reason string }

func (e *ForbiddenError) Error() string { return e.Reason }

func forbidden(format string, args ...any) error {
	return &ForbiddenError{Reason: fmt.Sprintf(format, args...)}
}

// IsForbidden reports whether err is an access refusal.
func IsForbidden(err error) bool {
	var f *ForbiddenError
	return errors.As(err, &f)
}

// ErrUnauthenticated is returned when a request carries no valid session.
var ErrUnauthenticated = errors.New("not signed in")

type userKey struct{}

// WithUser returns ctx carrying the signed-in user.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the signed-in user carried by ctx.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// PolicyReader reads the whole placement policy as one snapshot.
type PolicyReader interface {
	Policy() (Policy, error)
}

// ClientWriter creates, renames and removes clients.
type ClientWriter interface {
	SaveClient(c Client) error
	DeleteClient(id string) error
}

// LinkWriter assigns repos to clients.
type LinkWriter interface {
	SaveLink(l RepoLink) error
	DeleteLink(owner, repo string) error
}

// EnvRuleWriter sets where a client's environments deploy.
type EnvRuleWriter interface {
	SaveEnvRule(r EnvRule) error
	DeleteEnvRule(clientID, environment string) error
}

// RepoRuleWriter sets per-repo environment overrides.
type RepoRuleWriter interface {
	SaveRepoRule(r RepoRule) error
	DeleteRepoRule(owner, repo, environment string) error
}

// PolicyStore is the full policy contract, composed from the small ones.
type PolicyStore interface {
	PolicyReader
	ClientWriter
	LinkWriter
	EnvRuleWriter
	RepoRuleWriter
}

// Installation is one GitHub account the mountabo App is installed on.
type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	Type    string `json:"type"` // "Organization" | "User"
}

// InstallationLister lists the accounts the App is installed on.
type InstallationLister interface {
	Installations(ctx context.Context) ([]Installation, error)
}

// RepoAccessReader returns the repos a user can reach, as last synced.
type RepoAccessReader interface {
	RepoAccessFor(userID int64) ([]RepoAccess, error)
}

// AccessService answers "may this person do this" for every route. It owns the
// placement rules; GitHub owns repo permissions.
type AccessService struct {
	policy      PolicyStore
	servers     ServerStore
	repos       RepoAccessReader
	deployments DeploymentStore
	admins      []string
}

// NewAccessService wires the access policy to its stores. admins (GitHub
// logins) are who approve deploys to environments that require approval.
func NewAccessService(policy PolicyStore, servers ServerStore, repos RepoAccessReader, deployments DeploymentStore, admins []string) *AccessService {
	return &AccessService{policy: policy, servers: servers, repos: repos, deployments: deployments, admins: admins}
}

// Permission is u's permission on owner/repo. Admins hold every permission.
func (a *AccessService) Permission(u User, owner, repo string) (Permission, error) {
	if u.IsAdmin {
		return PermAdmin, nil
	}
	list, err := a.repos.RepoAccessFor(u.ID)
	if err != nil {
		return PermNone, fmt.Errorf("read repo access: %w", err)
	}
	for _, r := range list {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Repo, repo) {
			return r.Permission, nil
		}
	}
	return PermNone, nil
}

// RequireRepo refuses unless u holds at least want on owner/repo.
func (a *AccessService) RequireRepo(u User, owner, repo string, want Permission) error {
	got, err := a.Permission(u, owner, repo)
	if err != nil {
		return err
	}
	if got < want {
		if want >= PermWrite {
			return forbidden("you need write access to %s/%s on GitHub to do this", owner, repo)
		}
		return forbidden("you don't have access to %s/%s on GitHub", owner, repo)
	}
	return nil
}

// CheckDeploy decides whether u may deploy owner/repo's environment onto
// serverID. Client isolation binds admins too: a client's server only ever
// runs that client's code.
func (a *AccessService) CheckDeploy(u User, owner, repo, environment, serverID string) error {
	if err := a.RequireRepo(u, owner, repo, PermWrite); err != nil {
		return err
	}
	server, err := a.servers.Get(serverID)
	if err != nil {
		return err
	}
	policy, err := a.policy.Policy()
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	client := policy.ClientFor(owner, repo)
	if server.ClientID != "" && server.ClientID != client {
		return forbidden("server %s belongs to client %q; %s/%s is not one of its projects", server.Name, server.ClientID, owner, repo)
	}
	if u.IsAdmin {
		return nil
	}
	target, ok := policy.TargetFor(owner, repo, environment)
	if !ok {
		return forbidden("no server is configured for %s/%s %q yet; ask an admin to add an environment rule", owner, repo, environment)
	}
	if target.ServerID != serverID {
		return forbidden("%s/%s %q deploys to a different server", owner, repo, environment)
	}
	return nil
}

// requiresApproval reports whether owner/repo's environment is gated on an admin.
func (a *AccessService) requiresApproval(owner, repo, environment string) (bool, error) {
	policy, err := a.policy.Policy()
	if err != nil {
		return false, fmt.Errorf("read policy: %w", err)
	}
	t, ok := policy.TargetFor(owner, repo, environment)
	return ok && t.RequireApproval, nil
}

// RequireChange decides whether u may change a live environment directly:
// rollback, redeploy, secrets, port, teardown. Those reach the server without
// a GitHub run, so on an environment that requires approval they would skip
// it; there they are admin-only.
func (a *AccessService) RequireChange(u User, owner, repo, environment string) error {
	if err := a.RequireRepo(u, owner, repo, PermWrite); err != nil {
		return err
	}
	if u.IsAdmin {
		return nil
	}
	gated, err := a.requiresApproval(owner, repo, environment)
	if err != nil {
		return err
	}
	if gated {
		return forbidden("%s/%s %q requires an admin's approval; ask an admin to make this change", owner, repo, environment)
	}
	return nil
}

// Approvers is who must approve deploy runs of owner/repo's environment: the
// admins when it requires approval, nobody otherwise.
func (a *AccessService) Approvers(owner, repo, environment string) ([]string, error) {
	gated, err := a.requiresApproval(owner, repo, environment)
	if err != nil || !gated {
		return nil, err
	}
	if len(a.admins) == 0 {
		return nil, fmt.Errorf("%s/%s %q requires approval but no admins are configured", owner, repo, environment)
	}
	return a.admins, nil
}

// TargetsView is what the deploy form needs for one repo: who owns it and where
// each configured environment goes.
type TargetsView struct {
	ClientID   string   `json:"clientId"`
	ClientName string   `json:"clientName"`
	Permission string   `json:"permission"`
	Targets    []Target `json:"targets"`
}

// Targets resolves owner/repo's configured environments for u.
func (a *AccessService) Targets(u User, owner, repo string) (TargetsView, error) {
	perm, err := a.Permission(u, owner, repo)
	if err != nil {
		return TargetsView{}, err
	}
	if perm < PermRead {
		return TargetsView{}, forbidden("you don't have access to %s/%s on GitHub", owner, repo)
	}
	policy, err := a.policy.Policy()
	if err != nil {
		return TargetsView{}, fmt.Errorf("read policy: %w", err)
	}
	clientID := policy.ClientFor(owner, repo)
	view := TargetsView{ClientID: clientID, Permission: perm.String(), Targets: policy.TargetsFor(owner, repo)}
	for _, c := range policy.Clients {
		if c.ID == clientID {
			view.ClientName = c.Name
		}
	}
	return view, nil
}

// VisibleServers filters servers to those u has a reason to see: the targets of
// their repos' environments and the servers already running their repos.
// Admins see every server.
func (a *AccessService) VisibleServers(u User, servers []Server) ([]Server, error) {
	if u.IsAdmin {
		return servers, nil
	}
	allowed, err := a.visibleServerIDs(u)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(servers))
	for _, s := range servers {
		if allowed[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// CanSeeServer reports whether u may read serverID's non-sensitive details.
func (a *AccessService) CanSeeServer(u User, serverID string) (bool, error) {
	if u.IsAdmin {
		return true, nil
	}
	allowed, err := a.visibleServerIDs(u)
	if err != nil {
		return false, err
	}
	return allowed[serverID], nil
}

func (a *AccessService) visibleServerIDs(u User) (map[string]bool, error) {
	repos, err := a.repos.RepoAccessFor(u.ID)
	if err != nil {
		return nil, fmt.Errorf("read repo access: %w", err)
	}
	policy, err := a.policy.Policy()
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	allowed := map[string]bool{}
	for _, r := range repos {
		for _, t := range policy.TargetsFor(r.Owner, r.Repo) {
			allowed[t.ServerID] = true
		}
	}
	if deps, derr := a.deployments.List(); derr == nil {
		for _, d := range deps {
			for _, r := range repos {
				if strings.EqualFold(r.Owner, d.Owner) && strings.EqualFold(r.Repo, d.Repo) {
					allowed[d.ServerID] = true
				}
			}
		}
	}
	return allowed, nil
}

// RepoFilter returns a predicate that keeps the repos u can read; admins keep
// everything.
func (a *AccessService) RepoFilter(u User) (func(owner, repo string) bool, error) {
	if u.IsAdmin {
		return func(string, string) bool { return true }, nil
	}
	repos, err := a.repos.RepoAccessFor(u.ID)
	if err != nil {
		return nil, fmt.Errorf("read repo access: %w", err)
	}
	set := map[string]bool{}
	for _, r := range repos {
		if r.Permission >= PermRead {
			set[strings.ToLower(r.Owner+"/"+r.Repo)] = true
		}
	}
	return func(owner, repo string) bool { return set[strings.ToLower(owner+"/"+repo)] }, nil
}

// --- admin: policy management ----------------------------------------------

var clientIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Policy returns the current placement policy (admin view).
func (a *AccessService) Policy() (Policy, error) { return a.policy.Policy() }

// SaveClient creates or renames a client. The id is a short slug.
func (a *AccessService) SaveClient(c Client) error {
	c.ID, c.Name = strings.TrimSpace(c.ID), strings.TrimSpace(c.Name)
	if !clientIDPattern.MatchString(c.ID) {
		return forbidden("client id must be lowercase letters, digits and dashes (max 40)")
	}
	if c.Name == "" {
		return forbidden("client name is required")
	}
	return a.policy.SaveClient(c)
}

// DeleteClient removes a client, its links and rules. A client that still owns
// servers is refused, so no server is left claiming a client that is gone.
func (a *AccessService) DeleteClient(id string) error {
	if id == AgencyClientID {
		return forbidden("the agency client cannot be deleted")
	}
	servers, err := a.servers.List()
	if err != nil {
		return err
	}
	for _, s := range servers {
		if s.ClientID == id {
			return forbidden("client still owns server %s; reassign it first", s.Name)
		}
	}
	return a.policy.DeleteClient(id)
}

func (a *AccessService) requireClient(id string) error {
	p, err := a.policy.Policy()
	if err != nil {
		return err
	}
	for _, c := range p.Clients {
		if c.ID == id {
			return nil
		}
	}
	return forbidden("unknown client %q", id)
}

// SaveLink assigns a repo (or "*" for a whole owner) to a client.
func (a *AccessService) SaveLink(l RepoLink) error {
	l.Owner, l.Repo = strings.TrimSpace(l.Owner), strings.TrimSpace(l.Repo)
	if l.Owner == "" || l.Repo == "" {
		return forbidden("owner and repo are required (repo may be *)")
	}
	if err := a.requireClient(l.ClientID); err != nil {
		return err
	}
	return a.policy.SaveLink(l)
}

// DeleteLink unlinks a repo; it falls back to an owner-wide link or the agency.
func (a *AccessService) DeleteLink(owner, repo string) error { return a.policy.DeleteLink(owner, repo) }

// checkPlacement refuses a rule that would send a client's code to a server
// another client owns.
func (a *AccessService) checkPlacement(clientID, serverID string) error {
	s, err := a.servers.Get(serverID)
	if err != nil {
		return err
	}
	if s.ClientID != "" && s.ClientID != clientID {
		return forbidden("server %s belongs to client %q", s.Name, s.ClientID)
	}
	return nil
}

// SaveEnvRule points one of a client's environments at a server.
func (a *AccessService) SaveEnvRule(r EnvRule) error {
	r.Environment = strings.TrimSpace(r.Environment)
	if r.Environment == "" {
		return forbidden("environment is required")
	}
	if err := a.requireClient(r.ClientID); err != nil {
		return err
	}
	if err := a.checkPlacement(r.ClientID, r.ServerID); err != nil {
		return err
	}
	return a.policy.SaveEnvRule(r)
}

// DeleteEnvRule removes a client's environment rule.
func (a *AccessService) DeleteEnvRule(clientID, environment string) error {
	return a.policy.DeleteEnvRule(clientID, environment)
}

// SaveRepoRule overrides one repo's environment.
func (a *AccessService) SaveRepoRule(r RepoRule) error {
	r.Owner, r.Repo, r.Environment = strings.TrimSpace(r.Owner), strings.TrimSpace(r.Repo), strings.TrimSpace(r.Environment)
	if r.Owner == "" || r.Repo == "" || r.Repo == "*" || r.Environment == "" {
		return forbidden("owner, repo and environment are required")
	}
	p, err := a.policy.Policy()
	if err != nil {
		return err
	}
	if err := a.checkPlacement(p.ClientFor(r.Owner, r.Repo), r.ServerID); err != nil {
		return err
	}
	return a.policy.SaveRepoRule(r)
}

// DeleteRepoRule removes a repo override.
func (a *AccessService) DeleteRepoRule(owner, repo, environment string) error {
	return a.policy.DeleteRepoRule(owner, repo, environment)
}

// SetServerClient hands a server to a client ("" for the agency's shared
// pool). It is refused while the server runs another client's deployments, so
// ownership can never be changed out from under running code.
func (a *AccessService) SetServerClient(serverID, clientID string) error {
	if clientID == AgencyClientID {
		clientID = ""
	}
	if clientID != "" {
		if err := a.requireClient(clientID); err != nil {
			return err
		}
	}
	server, err := a.servers.Get(serverID)
	if err != nil {
		return err
	}
	if clientID != "" {
		p, err := a.policy.Policy()
		if err != nil {
			return err
		}
		deps, err := a.deployments.List()
		if err != nil {
			return err
		}
		for _, d := range deps {
			if d.ServerID == serverID && p.ClientFor(d.Owner, d.Repo) != clientID {
				return forbidden("server runs %s/%s, which is not a %q project; remove that deployment first", d.Owner, d.Repo, clientID)
			}
		}
	}
	server.ClientID = clientID
	return a.servers.Save(server)
}
