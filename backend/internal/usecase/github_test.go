package usecase

import (
	"context"
	"errors"
	"testing"
)

// fakeRepoLister, fakePortDetector, fakeStore and fakeUserTokens are in-memory
// stand-ins for the connector's ports, letting it be tested without GitHub.
type fakeRepoLister struct {
	repos []Repo
	err   error
	calls int
	seen  []Token
}

func (f *fakeRepoLister) List(_ context.Context, t Token) ([]Repo, error) {
	f.calls++
	f.seen = append(f.seen, t)
	return f.repos, f.err
}

type fakePortDetector struct {
	ports    []ServicePort
	strategy DeployStrategy
	err      error
	seen     Token
}

func (f *fakePortDetector) DetectPorts(_ context.Context, t Token, _ RepoRef) ([]ServicePort, DeployStrategy, error) {
	f.seen = t
	return f.ports, f.strategy, f.err
}

// fakeStore serves installation tokens (RepoTokens), recording which owner each
// was minted for.
type fakeStore struct {
	loadTok Token
	loadErr error
	owners  []string
}

func (f *fakeStore) TokenFor(_ context.Context, owner string) (Token, error) {
	f.owners = append(f.owners, owner)
	return f.loadTok, f.loadErr
}

type fakeUserTokens map[int64]Token

func (f fakeUserTokens) UserToken(_ context.Context, u User) (Token, error) {
	t, ok := f[u.ID]
	if !ok {
		return Token{}, errors.New("no token")
	}
	return t, nil
}

// Repos are read with the signed-in user's own token (so GitHub decides what
// they see) and cached per user, never shared between users.
func TestRepositories_UsesEachUsersOwnTokenAndCachesPerUser(t *testing.T) {
	lister := &fakeRepoLister{repos: []Repo{{FullName: "acme/shop", Permission: PermWrite}}}
	users := fakeUserTokens{1: {AccessToken: "alice"}, 2: {AccessToken: "bob"}}
	c := NewGitHubConnector(lister, &fakePortDetector{}, &fakeStore{}, users)

	for range 2 {
		if _, err := c.Repositories(context.Background(), User{ID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Repositories(context.Background(), User{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if lister.calls != 2 {
		t.Fatalf("want one GitHub call per user (cached), got %d", lister.calls)
	}
	if lister.seen[0].AccessToken != "alice" || lister.seen[1].AccessToken != "bob" {
		t.Errorf("tokens used = %+v, want alice then bob", lister.seen)
	}
}

func TestRepositories_PropagatesTokenFailure(t *testing.T) {
	c := NewGitHubConnector(&fakeRepoLister{}, &fakePortDetector{}, &fakeStore{}, fakeUserTokens{})
	if _, err := c.Repositories(context.Background(), User{ID: 9}); err == nil {
		t.Fatal("want an error when the user has no token")
	}
}

// Port detection runs as the App installation of the repo's owner.
func TestDetectPorts_UsesTheOwnersInstallationToken(t *testing.T) {
	store := &fakeStore{loadTok: Token{AccessToken: "inst"}}
	ports := &fakePortDetector{ports: []ServicePort{{Container: "3000"}}, strategy: StrategyDocker}
	c := NewGitHubConnector(&fakeRepoLister{}, ports, store, fakeUserTokens{})

	got, strategy, err := c.DetectPorts(context.Background(), RepoRef{Owner: "acme", Name: "shop"})
	if err != nil || len(got) != 1 || strategy != StrategyDocker {
		t.Fatalf("DetectPorts = %v %q %v", got, strategy, err)
	}
	if len(store.owners) != 1 || store.owners[0] != "acme" || ports.seen.AccessToken != "inst" {
		t.Errorf("owners=%v token=%q; want acme / inst", store.owners, ports.seen.AccessToken)
	}
}

func TestDetectPorts_AppNotInstalled(t *testing.T) {
	c := NewGitHubConnector(&fakeRepoLister{}, &fakePortDetector{}, &fakeStore{loadErr: ErrNotConnected}, fakeUserTokens{})
	if _, _, err := c.DetectPorts(context.Background(), RepoRef{Owner: "x", Name: "y"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}
