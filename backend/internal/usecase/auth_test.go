package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeAuthorizer struct {
	tok       Token
	refreshed int
}

func (f *fakeAuthorizer) Exchange(context.Context, string, string) (Token, error) { return f.tok, nil }
func (f *fakeAuthorizer) Refresh(_ context.Context, _ Token) (Token, error) {
	f.refreshed++
	return Token{AccessToken: "fresh", RefreshToken: "r2", Expiry: time.Now().Add(8 * time.Hour)}, nil
}

type fakeDirectory struct {
	user  GitHubUser
	repos []RepoAccess
	err   error
	calls int
}

func (f *fakeDirectory) Identity(context.Context, Token) (GitHubUser, error) { return f.user, nil }
func (f *fakeDirectory) UserRepos(context.Context, Token) ([]RepoAccess, error) {
	f.calls++
	return f.repos, f.err
}

// memAuthStore is an in-memory UserStore, SessionStore and RepoAccessWriter.
type memAuthStore struct {
	users    map[int64]User
	sessions map[string]int64
	access   map[int64][]RepoAccess
}

func newMemAuthStore() *memAuthStore {
	return &memAuthStore{users: map[int64]User{}, sessions: map[string]int64{}, access: map[int64][]RepoAccess{}}
}

func (m *memAuthStore) UpsertUser(u User) (User, error) {
	for id, existing := range m.users {
		if existing.GitHubID == u.GitHubID {
			u.ID, u.ReposSyncedAt, u.RevokedAt = id, existing.ReposSyncedAt, existing.RevokedAt
			m.users[id] = u
			return u, nil
		}
	}
	u.ID = int64(len(m.users) + 1)
	m.users[u.ID] = u
	return u, nil
}
func (m *memAuthStore) GetUser(id int64) (User, error) {
	u, ok := m.users[id]
	if !ok {
		return User{}, errors.New("no user")
	}
	return u, nil
}
func (m *memAuthStore) ListUsers() ([]User, error) { return nil, nil }
func (m *memAuthStore) CreateSession(h []byte, id int64, _ time.Time) error {
	m.sessions[string(h)] = id
	return nil
}
func (m *memAuthStore) SessionUser(h []byte, _ time.Time) (int64, error) {
	id, ok := m.sessions[string(h)]
	if !ok {
		return 0, errors.New("no session")
	}
	return id, nil
}
func (m *memAuthStore) DeleteSession(h []byte) error { delete(m.sessions, string(h)); return nil }
func (m *memAuthStore) DeleteUserSessions(id int64) error {
	for h, uid := range m.sessions {
		if uid == id {
			delete(m.sessions, h)
		}
	}
	return nil
}
func (m *memAuthStore) SetRevoked(id int64, at *time.Time) error {
	u, ok := m.users[id]
	if !ok {
		return errors.New("no user")
	}
	u.RevokedAt = at
	m.users[id] = u
	return nil
}
func (m *memAuthStore) ReplaceRepoAccess(id int64, r []RepoAccess, at time.Time) error {
	m.access[id] = r
	u := m.users[id]
	u.ReposSyncedAt = at
	m.users[id] = u
	return nil
}

func newAuth(dir *fakeDirectory, auth *fakeAuthorizer, store *memAuthStore, admins ...string) (*AuthService, *fakeVault) {
	vault := newFakeVault()
	return NewAuthService(auth, dir, store, store, store, vault, nil, admins, slog.New(slog.NewTextHandler(io.Discard, nil))), vault
}

func TestLogin_OutsideCollaboratorWithRepoAccessGetsIn(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 7, Login: "contractor"}, repos: []RepoAccess{{Owner: "acme-org", Repo: "shop", Permission: PermWrite}}}
	store := newMemAuthStore()
	svc, _ := newAuth(dir, &fakeAuthorizer{tok: Token{AccessToken: "u"}}, store, "boss")

	sess, err := svc.Login(context.Background(), "code", "uri")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.Token == "" || sess.User.IsAdmin {
		t.Fatalf("session = %+v; want a token for a non-admin", sess)
	}
	u, err := svc.Authenticate(context.Background(), sess.Token)
	if err != nil || u.Login != "contractor" {
		t.Fatalf("Authenticate = %+v, %v", u, err)
	}
	if len(store.access[u.ID]) != 1 {
		t.Errorf("repo access not cached: %+v", store.access)
	}
	// The raw token is never what is stored.
	if _, raw := store.sessions[sess.Token]; raw {
		t.Error("session stored by raw token; want only its hash")
	}
}

func TestLogin_NoReposAndNotAdminIsRefused(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 8, Login: "stranger"}}
	svc, _ := newAuth(dir, &fakeAuthorizer{}, newMemAuthStore(), "boss")
	if _, err := svc.Login(context.Background(), "c", "u"); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("want ErrNoAccess, got %v", err)
	}
}

func TestLogin_AdminWithoutReposGetsIn(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 1, Login: "Boss"}}
	svc, _ := newAuth(dir, &fakeAuthorizer{}, newMemAuthStore(), "boss")
	sess, err := svc.Login(context.Background(), "c", "u")
	if err != nil || !sess.User.IsAdmin {
		t.Fatalf("admin login = %+v, %v (logins match case-insensitively)", sess.User, err)
	}
}

func TestAuthenticate_RejectsUnknownAndLoggedOutSessions(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 7, Login: "dev"}, repos: []RepoAccess{{Owner: "a", Repo: "b", Permission: PermRead}}}
	svc, _ := newAuth(dir, &fakeAuthorizer{}, newMemAuthStore())
	if _, err := svc.Authenticate(context.Background(), "nope"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("unknown token: %v", err)
	}
	sess, _ := svc.Login(context.Background(), "c", "u")
	_ = svc.Logout(sess.Token)
	if _, err := svc.Authenticate(context.Background(), sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("logged-out token: %v", err)
	}
}

// Losing a repo on GitHub takes effect at the next sync; GitHub being down
// keeps cached access only within the grace window.
func TestAuthenticate_ResyncsStaleAccess(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 7, Login: "dev"}, repos: []RepoAccess{{Owner: "a", Repo: "b", Permission: PermWrite}}}
	store := newMemAuthStore()
	auth := &fakeAuthorizer{tok: Token{AccessToken: "u", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}}
	svc, _ := newAuth(dir, auth, store)
	sess, _ := svc.Login(context.Background(), "c", "u")
	clock := time.Now()
	svc.now = func() time.Time { return clock }

	// Still fresh: no GitHub call.
	before := dir.calls
	if _, err := svc.Authenticate(context.Background(), sess.Token); err != nil || dir.calls != before {
		t.Fatalf("fresh session should not resync (calls %d->%d, err %v)", before, dir.calls, err)
	}

	// Stale: resync picks up the removal (and refreshes the expired token).
	clock = clock.Add(2 * time.Hour)
	dir.repos = nil
	u, err := svc.Authenticate(context.Background(), sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.access[u.ID]) != 0 || auth.refreshed != 1 {
		t.Errorf("access=%v refreshed=%d; want access cleared and one token refresh", store.access[u.ID], auth.refreshed)
	}

	// GitHub down past the grace window: refuse.
	dir.err = errors.New("github down")
	clock = clock.Add(2 * time.Hour)
	if _, err := svc.Authenticate(context.Background(), sess.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("stale beyond grace with GitHub down should refuse, got %v", err)
	}
}
