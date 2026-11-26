package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeHome struct {
	login string
	err   error
}

func (f fakeHome) Owner(context.Context) (string, error) { return f.login, f.err }

func newHomeAuth(dir *fakeDirectory, home HomeAccount, admins ...string) (*AuthService, *memAuthStore) {
	store := newMemAuthStore()
	svc := NewAuthService(&fakeAuthorizer{tok: Token{AccessToken: "u"}}, dir, store, store, store, newFakeVault(), home, admins, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, store
}

// The App is public, so anyone can install it on their own account. Repos there
// must not let a stranger in.
func TestLogin_StrangerWhoInstalledTheAppOnTheirOwnReposIsRefused(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 9, Login: "stranger"}, repos: []RepoAccess{{Owner: "stranger", Repo: "mine", Permission: PermAdmin}}}
	svc, _ := newHomeAuth(dir, fakeHome{login: "Agency"})

	if _, err := svc.Login(context.Background(), "code", "uri"); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("Login err = %v, want ErrNoAccess", err)
	}
}

// A collaborator invited to one of the home account's repos gets in, and only
// the home account's repos are kept (the owner match ignores case).
func TestLogin_CollaboratorOnAHomeRepoGetsInWithOnlyHomeRepos(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 7, Login: "contractor"}, repos: []RepoAccess{
		{Owner: "agency", Repo: "meals-fe", Permission: PermWrite},
		{Owner: "contractor", Repo: "side-project", Permission: PermAdmin},
	}}
	svc, store := newHomeAuth(dir, fakeHome{login: "Agency"})

	sess, err := svc.Login(context.Background(), "code", "uri")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	got := store.access[sess.User.ID]
	if len(got) != 1 || got[0].Repo != "meals-fe" {
		t.Fatalf("repo access = %+v, want only agency/meals-fe", got)
	}
}

// If GitHub can't say who owns the App, nobody is let in on repo access alone.
func TestLogin_FailsClosedWhenTheOwnerCannotBeRead(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 7, Login: "contractor"}, repos: []RepoAccess{{Owner: "agency", Repo: "meals-fe", Permission: PermWrite}}}
	svc, _ := newHomeAuth(dir, fakeHome{err: errors.New("github down")})

	if _, err := svc.Login(context.Background(), "code", "uri"); err == nil {
		t.Fatal("Login succeeded without knowing the App's owner; want an error")
	}
}

// Admins in MOUNTABO_ADMINS still get in with no home repos, as before.
func TestLogin_AdminWithoutHomeReposStillGetsIn(t *testing.T) {
	dir := &fakeDirectory{user: GitHubUser{ID: 1, Login: "boss"}, repos: []RepoAccess{{Owner: "boss", Repo: "dotfiles", Permission: PermAdmin}}}
	svc, store := newHomeAuth(dir, fakeHome{login: "agency"}, "boss")

	sess, err := svc.Login(context.Background(), "code", "uri")
	if err != nil || !sess.User.IsAdmin {
		t.Fatalf("Login = %+v, %v; want an admin session", sess, err)
	}
	if len(store.access[sess.User.ID]) != 0 {
		t.Errorf("repo access = %+v, want none outside the home account", store.access[sess.User.ID])
	}
}

// The repo picker shows only the home account's repos.
func TestRepositories_OnlyHomeAccountRepos(t *testing.T) {
	lister := &fakeRepoLister{repos: []Repo{{Owner: "Agency", Name: "meals-fe"}, {Owner: "stranger", Name: "mine"}}}
	c := NewGitHubConnector(lister, &fakePortDetector{}, &fakeStore{}, fakeUserTokens{1: {AccessToken: "u"}}, fakeHome{login: "agency"})

	got, err := c.Repositories(context.Background(), User{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "meals-fe" {
		t.Fatalf("repos = %+v, want only Agency/meals-fe", got)
	}
}
