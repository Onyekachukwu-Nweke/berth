package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	// sessionTTL is how long a sign-in lasts before GitHub is asked again.
	sessionTTL = 7 * 24 * time.Hour
	// repoSyncInterval is how stale a user's cached repo access may get before
	// it is re-read from GitHub. Removing someone from a repo on GitHub takes
	// effect in berth within this window.
	repoSyncInterval = 10 * time.Minute
	// repoSyncGrace is how long cached access keeps working while GitHub cannot
	// be reached; past it the session is refused rather than trusted blindly.
	repoSyncGrace = time.Hour
)

// ErrNoAccess is returned at sign-in when the person can reach no repo the App
// is installed on and is not an admin.
var ErrNoAccess = errors.New("no repositories available")

// ErrRevoked is returned at sign-in for someone an admin removed.
var ErrRevoked = errors.New("access removed by an admin")

// GitHubUser is the identity GitHub reports for a user token.
type GitHubUser struct {
	ID        int64
	Login     string
	Name      string
	AvatarURL string
}

// UserAuthorizer runs the GitHub App's user sign-in: it trades a code for a
// user token and renews expired ones.
type UserAuthorizer interface {
	Exchange(ctx context.Context, code, redirectURI string) (Token, error)
	Refresh(ctx context.Context, t Token) (Token, error)
}

// UserDirectory answers who a user token belongs to and which repos (shared
// with the App) that person can reach, with their permission on each.
type UserDirectory interface {
	Identity(ctx context.Context, t Token) (GitHubUser, error)
	UserRepos(ctx context.Context, t Token) ([]RepoAccess, error)
}

// UserStore persists users.
type UserStore interface {
	UpsertUser(u User) (User, error)
	GetUser(id int64) (User, error)
	ListUsers() ([]User, error)
	SetRevoked(userID int64, at *time.Time) error
}

// SessionStore persists sessions by the hash of their token.
type SessionStore interface {
	CreateSession(hash []byte, userID int64, expires time.Time) error
	SessionUser(hash []byte, now time.Time) (int64, error)
	DeleteSession(hash []byte) error
	DeleteUserSessions(userID int64) error
}

// RepoAccessWriter replaces a user's cached repo access.
type RepoAccessWriter interface {
	ReplaceRepoAccess(userID int64, repos []RepoAccess, at time.Time) error
}

// AuthService signs people in with GitHub and resolves sessions back to users,
// keeping each user's repo access fresh against GitHub.
type AuthService struct {
	authorizer UserAuthorizer
	directory  UserDirectory
	users      UserStore
	sessions   SessionStore
	access     RepoAccessWriter
	vault      SecretVault
	gate       RepoGate
	admins     map[string]bool
	log        *slog.Logger
	now        func() time.Time
}

// NewAuthService wires sign-in. admins are GitHub logins that are always
// admins (BERTH_ADMINS); everyone else is a developer, who may sign in only
// with a repo the gate allows (nil allows any).
func NewAuthService(authorizer UserAuthorizer, directory UserDirectory, users UserStore, sessions SessionStore, access RepoAccessWriter, vault SecretVault, gate RepoGate, admins []string, log *slog.Logger) *AuthService {
	set := map[string]bool{}
	for _, a := range admins {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			set[a] = true
		}
	}
	return &AuthService{authorizer: authorizer, directory: directory, users: users, sessions: sessions, access: access, vault: vault, gate: gate, admins: set, log: log, now: time.Now}
}

func (s *AuthService) isAdmin(login string) bool { return s.admins[strings.ToLower(login)] }

func userTokenKey(userID int64) string { return fmt.Sprintf("user/%d/github-token", userID) }

func hashSession(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Session is a fresh sign-in: the opaque token for the cookie and its expiry.
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      User      `json:"user"`
}

// Login completes GitHub sign-in: identify the person, read the repos they can
// reach through the App, and open a session. A developer needs a repo inside the
// deploy sources, and must not have been removed; admins always get in. Every
// repo GitHub reports is stored, so a change to the sources applies at once.
func (s *AuthService) Login(ctx context.Context, code, redirectURI string) (Session, error) {
	tok, err := s.authorizer.Exchange(ctx, code, redirectURI)
	if err != nil {
		return Session{}, fmt.Errorf("exchange code: %w", err)
	}
	gh, err := s.directory.Identity(ctx, tok)
	if err != nil {
		return Session{}, fmt.Errorf("read identity: %w", err)
	}
	repos, err := s.directory.UserRepos(ctx, tok)
	if err != nil {
		return Session{}, fmt.Errorf("read repositories: %w", err)
	}
	admin := s.isAdmin(gh.Login)
	if !admin {
		allowed, err := s.allowed(ctx, repos)
		if err != nil {
			return Session{}, err
		}
		if !anyReadable(allowed) {
			return Session{}, ErrNoAccess
		}
	}

	now := s.now()
	user, err := s.users.UpsertUser(User{
		GitHubID: gh.ID, Login: gh.Login, Name: gh.Name, AvatarURL: gh.AvatarURL,
		IsAdmin: admin, LastLoginAt: now,
	})
	if err != nil {
		return Session{}, fmt.Errorf("save user: %w", err)
	}
	if user.RevokedAt != nil && !admin {
		return Session{}, ErrRevoked
	}
	if err := s.access.ReplaceRepoAccess(user.ID, repos, now); err != nil {
		return Session{}, fmt.Errorf("save repo access: %w", err)
	}
	if err := s.saveUserToken(user.ID, tok); err != nil {
		return Session{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, fmt.Errorf("session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := now.Add(sessionTTL)
	if err := s.sessions.CreateSession(hashSession(token), user.ID, expires); err != nil {
		return Session{}, fmt.Errorf("save session: %w", err)
	}
	return Session{Token: token, ExpiresAt: expires, User: user}, nil
}

// allowed keeps the repos the gate lets developers use.
func (s *AuthService) allowed(ctx context.Context, repos []RepoAccess) ([]RepoAccess, error) {
	if s.gate == nil {
		return repos, nil
	}
	allow, err := s.gate.DeployGate(ctx)
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

func anyReadable(repos []RepoAccess) bool {
	for _, r := range repos {
		if r.Permission >= PermRead {
			return true
		}
	}
	return false
}

// Authenticate resolves a session token to its user, re-reading the user's repo
// access from GitHub when it is stale. Admin status follows BERTH_ADMINS on
// every request, so removing an admin takes effect immediately.
func (s *AuthService) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrUnauthenticated
	}
	id, err := s.sessions.SessionUser(hashSession(token), s.now())
	if err != nil {
		return User{}, ErrUnauthenticated
	}
	user, err := s.users.GetUser(id)
	if err != nil {
		return User{}, ErrUnauthenticated
	}
	user.IsAdmin = s.isAdmin(user.Login)
	if user.RevokedAt != nil && !user.IsAdmin {
		return User{}, ErrUnauthenticated
	}

	if s.now().Sub(user.ReposSyncedAt) > repoSyncInterval {
		if err := s.syncRepos(ctx, user); err != nil {
			if s.now().Sub(user.ReposSyncedAt) > repoSyncGrace {
				s.log.Warn("repo access could not be refreshed; refusing session", "user", user.Login, "err", err)
				return User{}, ErrUnauthenticated
			}
			s.log.Warn("repo access refresh failed; using cached access", "user", user.Login, "err", err)
		}
	}
	return user, nil
}

func (s *AuthService) syncRepos(ctx context.Context, user User) error {
	tok, err := s.UserToken(ctx, user)
	if err != nil {
		return err
	}
	repos, err := s.directory.UserRepos(ctx, tok)
	if err != nil {
		return fmt.Errorf("read repositories: %w", err)
	}
	return s.access.ReplaceRepoAccess(user.ID, repos, s.now())
}

// UserToken returns user's GitHub token, renewing it first if it has expired.
func (s *AuthService) UserToken(ctx context.Context, user User) (Token, error) {
	blob, err := s.vault.LoadSecret(userTokenKey(user.ID))
	if err != nil {
		return Token{}, fmt.Errorf("load user token: %w", err)
	}
	var tok Token
	if err := json.Unmarshal([]byte(blob), &tok); err != nil {
		return Token{}, fmt.Errorf("decode user token: %w", err)
	}
	if !tok.Expiry.IsZero() && s.now().Add(time.Minute).After(tok.Expiry) {
		fresh, err := s.authorizer.Refresh(ctx, tok)
		if err != nil {
			return Token{}, fmt.Errorf("refresh user token: %w", err)
		}
		if err := s.saveUserToken(user.ID, fresh); err != nil {
			return Token{}, err
		}
		tok = fresh
	}
	return tok, nil
}

func (s *AuthService) saveUserToken(userID int64, tok Token) error {
	blob, err := json.Marshal(tok) //nolint:gosec // sealed into the vault; never logged
	if err != nil {
		return fmt.Errorf("encode user token: %w", err)
	}
	if err := s.vault.SaveSecret(userTokenKey(userID), string(blob)); err != nil {
		return fmt.Errorf("save user token: %w", err)
	}
	return nil
}

// Logout ends a session. It is idempotent.
func (s *AuthService) Logout(token string) error {
	if token == "" {
		return nil
	}
	return s.sessions.DeleteSession(hashSession(token))
}

// Users lists everyone who has signed in (admin view), with admin status as
// currently configured.
func (s *AuthService) Users() ([]User, error) {
	users, err := s.users.ListUsers()
	if err != nil {
		return nil, err
	}
	for i := range users {
		users[i].IsAdmin = s.isAdmin(users[i].Login)
	}
	return users, nil
}

// Remove signs someone out everywhere, forgets their GitHub token and repo
// access, and stops them signing in until an admin restores them. Admins in
// BERTH_ADMINS can't be removed here, and nobody can remove themselves.
func (s *AuthService) Remove(actor User, userID int64) error {
	if actor.ID == userID {
		return forbidden("you can't remove yourself")
	}
	target, err := s.users.GetUser(userID)
	if err != nil {
		return forbidden("no such person")
	}
	if s.isAdmin(target.Login) {
		return forbidden("%s is an admin through BERTH_ADMINS; take them out of it first", target.Login)
	}
	now := s.now()
	if err := s.users.SetRevoked(userID, &now); err != nil {
		return fmt.Errorf("mark removed: %w", err)
	}
	if err := s.sessions.DeleteUserSessions(userID); err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	if err := s.vault.DeleteSecret(userTokenKey(userID)); err != nil {
		s.log.Warn("could not delete a removed user's GitHub token", "user", target.Login, "err", err)
	}
	if err := s.access.ReplaceRepoAccess(userID, nil, now); err != nil {
		return fmt.Errorf("clear repo access: %w", err)
	}
	s.log.Info("user removed", "user", target.Login, "by", actor.Login)
	return nil
}

// Restore lets a removed person sign in again. Their repo access is read
// fresh from GitHub when they do.
func (s *AuthService) Restore(actor User, userID int64) error {
	target, err := s.users.GetUser(userID)
	if err != nil {
		return forbidden("no such person")
	}
	if err := s.users.SetRevoked(userID, nil); err != nil {
		return fmt.Errorf("restore user: %w", err)
	}
	s.log.Info("user restored", "user", target.Login, "by", actor.Login)
	return nil
}
