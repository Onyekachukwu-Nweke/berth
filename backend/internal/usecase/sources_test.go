package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type fakeHome struct {
	login string
	err   error
}

func (f fakeHome) Owner(context.Context) (string, error) { return f.login, f.err }

// sourcesFixture: the App belongs to "agency". contractor (id 1 after sign-in)
// is a collaborator on agency/meals-fe and also has repos of their own, which
// the public App could be installed on.
type sourcesFixture struct {
	policy *memPolicy
	access *AccessService
	auth   *AuthService
	store  *memAuthStore
	vault  *fakeVault
	dir    *fakeDirectory
}

func newSourcesFixture(home HomeAccount, admins ...string) *sourcesFixture {
	f := &sourcesFixture{policy: &memPolicy{}, store: newMemAuthStore(), vault: newFakeVault()}
	f.dir = &fakeDirectory{user: GitHubUser{ID: 7, Login: "contractor"}, repos: []RepoAccess{
		{Owner: "agency", Repo: "meals-fe", Permission: PermWrite},
		{Owner: "agency", Repo: "payroll", Permission: PermWrite},
		{Owner: "contractor", Repo: "side-project", Permission: PermAdmin},
	}}
	f.access = NewAccessService(f.policy, newMemServerStore(), storeAccess{f.store}, &fakeDeploymentStore{}, home)
	f.auth = NewAuthService(&fakeAuthorizer{tok: Token{AccessToken: "u"}}, f.dir, f.store, f.store, f.store, f.vault, f.access, admins, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

// storeAccess reads repo access from the auth store, so what sign-in stores is
// what the access checks see.
type storeAccess struct{ m *memAuthStore }

func (s storeAccess) RepoAccessFor(id int64) ([]RepoAccess, error) { return s.m.access[id], nil }

func (f *sourcesFixture) login(t *testing.T) Session {
	t.Helper()
	sess, err := f.auth.Login(context.Background(), "code", "uri")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return sess
}

func (f *sourcesFixture) perm(t *testing.T, u User, owner, repo string) Permission {
	t.Helper()
	p, err := f.access.Permission(u, owner, repo)
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	return p
}

// With no sources set, developers deploy only from the App owner's account.
// Their own repos don't count, even if they installed the public App on them.
func TestSources_DefaultIsTheAppOwnersAccount(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "Agency"})
	u := f.login(t).User

	if got := f.perm(t, u, "agency", "meals-fe"); got != PermWrite {
		t.Errorf("agency/meals-fe = %v, want write", got)
	}
	if got := f.perm(t, u, "contractor", "side-project"); got != PermNone {
		t.Errorf("own repo = %v, want none", got)
	}
}

// Someone whose only repos are their own can't sign in.
func TestSources_StrangerWithOnlyTheirOwnReposIsRefused(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"})
	f.dir.repos = []RepoAccess{{Owner: "stranger", Repo: "mine", Permission: PermAdmin}}

	if _, err := f.auth.Login(context.Background(), "code", "uri"); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("Login err = %v, want ErrNoAccess", err)
	}
}

// If GitHub can't say who owns the App, developers aren't let in on a guess.
func TestSources_FailClosedWhenTheOwnerCannotBeRead(t *testing.T) {
	f := newSourcesFixture(fakeHome{err: errors.New("github down")})
	if _, err := f.auth.Login(context.Background(), "code", "uri"); err == nil {
		t.Fatal("Login succeeded without knowing the App's owner")
	}
}

// A pattern narrows an account to matching repos, and a change applies to
// people already signed in, without waiting for a refresh.
func TestSources_PatternAppliesImmediately(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"})
	u := f.login(t).User

	if err := f.access.SaveSource(Source{Account: "Agency", Pattern: "meals-*"}); err != nil {
		t.Fatal(err)
	}
	if got := f.perm(t, u, "agency", "meals-fe"); got != PermWrite {
		t.Errorf("agency/meals-fe = %v, want write", got)
	}
	if got := f.perm(t, u, "agency", "payroll"); got != PermNone {
		t.Errorf("agency/payroll = %v, want none (outside meals-*)", got)
	}
	if err := f.access.RequireRepo(u, "agency", "payroll", PermWrite); err == nil || !strings.Contains(err.Error(), "ask an admin") {
		t.Errorf("refusal = %v, want it to say an admin hasn't allowed the repo", err)
	}

	if err := f.access.DeleteSource("agency", "meals-*"); err != nil {
		t.Fatal(err)
	}
	if got := f.perm(t, u, "agency", "payroll"); got != PermWrite {
		t.Errorf("after removing the pattern, agency/payroll = %v, want write (default back to the owner)", got)
	}
}

// Admins can add another account, e.g. a client's org.
func TestSources_AdminCanAddAnotherAccount(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"})
	f.dir.repos = append(f.dir.repos, RepoAccess{Owner: "acme-org", Repo: "shop", Permission: PermWrite})
	u := f.login(t).User

	if got := f.perm(t, u, "acme-org", "shop"); got != PermNone {
		t.Fatalf("acme-org/shop before = %v, want none", got)
	}
	_ = f.access.SaveSource(Source{Account: "agency", Pattern: "*"})
	_ = f.access.SaveSource(Source{Account: "acme-org"})
	if got := f.perm(t, u, "acme-org", "shop"); got != PermWrite {
		t.Errorf("acme-org/shop after = %v, want write", got)
	}
	if got := f.perm(t, u, "contractor", "side-project"); got != PermNone {
		t.Errorf("own repo = %v, want none", got)
	}
}

// Admins aren't bound by sources: they can deploy from their own account.
func TestSources_AdminsAreNotLimited(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"}, "boss")
	f.dir.user = GitHubUser{ID: 1, Login: "boss"}
	f.dir.repos = []RepoAccess{{Owner: "boss", Repo: "dotfiles", Permission: PermAdmin}}
	u := f.login(t).User
	if !u.IsAdmin {
		t.Fatal("want an admin session")
	}

	lister := &fakeRepoLister{repos: []Repo{{Owner: "boss", Name: "dotfiles"}}}
	c := NewGitHubConnector(lister, &fakePortDetector{}, &fakeStore{}, fakeUserTokens{u.ID: {AccessToken: "u"}}, f.access)
	got, err := c.Repositories(context.Background(), u)
	if err != nil || len(got) != 1 {
		t.Fatalf("admin repos = %+v, %v; want their own repo", got, err)
	}
}

// The repo picker shows developers only repos inside the sources.
func TestSources_RepoPickerForDevelopers(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"})
	lister := &fakeRepoLister{repos: []Repo{{Owner: "Agency", Name: "meals-fe"}, {Owner: "contractor", Name: "side-project"}}}
	c := NewGitHubConnector(lister, &fakePortDetector{}, &fakeStore{}, fakeUserTokens{1: {AccessToken: "u"}}, f.access)

	got, err := c.Repositories(context.Background(), User{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "meals-fe" {
		t.Fatalf("repos = %+v, want only Agency/meals-fe", got)
	}
}

func TestSources_SaveValidates(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"})
	for _, bad := range []Source{
		{Account: "not an account"},
		{Account: "agency", Pattern: "agency/meals-*"},
		{Account: "agency", Pattern: "meals-["},
	} {
		if err := f.access.SaveSource(bad); !IsForbidden(err) {
			t.Errorf("SaveSource(%+v) = %v, want a refusal", bad, err)
		}
	}
	if err := f.access.SaveSource(Source{Account: "Acme-Org"}); err != nil {
		t.Fatal(err)
	}
	if got := f.policy.p.Sources; len(got) != 1 || got[0] != (Source{Account: "acme-org", Pattern: "*"}) {
		t.Errorf("stored = %+v, want acme-org/*", got)
	}
}

// Removing someone signs them out, forgets their token and access, and keeps
// them out until they're restored.
func TestRemove_SignsOutAndBlocksUntilRestored(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"}, "boss")
	sess := f.login(t)
	id := sess.User.ID
	actor := User{ID: 99, Login: "boss", IsAdmin: true}

	if err := f.auth.Remove(actor, id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := f.auth.Authenticate(context.Background(), sess.Token); err == nil {
		t.Error("old session still works after removal")
	}
	if _, ok := f.vault.secrets[userTokenKey(id)]; ok {
		t.Error("GitHub token kept after removal")
	}
	if len(f.store.access[id]) != 0 {
		t.Errorf("repo access kept after removal: %+v", f.store.access[id])
	}
	if _, err := f.auth.Login(context.Background(), "code", "uri"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Login after removal = %v, want ErrRevoked", err)
	}

	if err := f.auth.Restore(actor, id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	f.login(t)
}

func TestRemove_RefusesSelfAndConfiguredAdmins(t *testing.T) {
	f := newSourcesFixture(fakeHome{login: "agency"}, "contractor")
	u := f.login(t).User // contractor is an admin here

	if err := f.auth.Remove(u, u.ID); !IsForbidden(err) {
		t.Errorf("removing yourself = %v, want a refusal", err)
	}
	if err := f.auth.Remove(User{ID: 99, Login: "boss", IsAdmin: true}, u.ID); !IsForbidden(err) {
		t.Errorf("removing a MOUNTABO_ADMINS admin = %v, want a refusal", err)
	}
}
