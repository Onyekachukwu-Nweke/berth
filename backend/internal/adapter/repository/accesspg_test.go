package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

func TestAccessSQL_UsersSessionsAndRepoAccess(t *testing.T) {
	s := NewAccessSQL(testDB(t))

	u, err := s.UpsertUser(usecase.User{GitHubID: 42, Login: "dev", Name: "Dev", LastLoginAt: time.Now()})
	if err != nil || u.ID == 0 {
		t.Fatalf("UpsertUser = %+v, %v", u, err)
	}
	// A renamed login is the same person (keyed by GitHub id).
	again, err := s.UpsertUser(usecase.User{GitHubID: 42, Login: "dev-renamed", LastLoginAt: time.Now()})
	if err != nil || again.ID != u.ID || again.Login != "dev-renamed" {
		t.Fatalf("re-upsert = %+v, %v", again, err)
	}

	hash := []byte("hash-1")
	if err := s.CreateSession(hash, u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if id, err := s.SessionUser(hash, time.Now()); err != nil || id != u.ID {
		t.Fatalf("SessionUser = %d, %v", id, err)
	}
	if _, err := s.SessionUser(hash, time.Now().Add(2*time.Hour)); err == nil {
		t.Error("expired session was accepted")
	}
	_ = s.DeleteSession(hash)
	if _, err := s.SessionUser(hash, time.Now()); err == nil {
		t.Error("deleted session was accepted")
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.ReplaceRepoAccess(u.ID, []usecase.RepoAccess{
		{Owner: "acme", Repo: "shop", Permission: usecase.PermWrite},
		{Owner: "acme", Repo: "none", Permission: usecase.PermNone},
	}, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.RepoAccessFor(u.ID)
	if err != nil || len(got) != 1 || got[0].Permission != usecase.PermWrite {
		t.Fatalf("RepoAccessFor = %+v, %v (no-permission rows are dropped)", got, err)
	}
	if fresh, _ := s.GetUser(u.ID); !fresh.ReposSyncedAt.Equal(at) {
		t.Errorf("repos_synced_at = %v, want %v", fresh.ReposSyncedAt, at)
	}
	// Replacing drops what GitHub no longer reports.
	_ = s.ReplaceRepoAccess(u.ID, nil, at)
	if got, _ := s.RepoAccessFor(u.ID); len(got) != 0 {
		t.Errorf("access not replaced: %+v", got)
	}
}

func TestAccessSQL_PolicyRoundTripAndCascade(t *testing.T) {
	db := testDB(t)
	s := NewAccessSQL(db)
	servers := NewServerSQL(db)
	_ = servers.Save(usecase.Server{ID: "srv", Name: "acme-prod", ClientID: "acme"})

	p, err := s.Policy()
	if err != nil || len(p.Clients) != 1 || p.Clients[0].ID != usecase.AgencyClientID {
		t.Fatalf("fresh policy should hold only the agency client: %+v, %v", p.Clients, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveClient(usecase.Client{ID: "acme", Name: "Acme"}))
	must(s.SaveLink(usecase.RepoLink{Owner: "Acme-Org", Repo: "*", ClientID: "acme"}))
	must(s.SaveEnvRule(usecase.EnvRule{ClientID: "acme", Environment: "Production", ServerID: "srv"}))
	must(s.SaveRepoRule(usecase.RepoRule{Owner: "acme-org", Repo: "shop", Environment: "staging", ServerID: "srv"}))

	p, _ = s.Policy()
	if len(p.Clients) != 2 || len(p.Links) != 1 || len(p.EnvRules) != 1 || len(p.RepoRules) != 1 {
		t.Fatalf("policy = %+v", p)
	}
	if p.Links[0].Owner != "acme-org" || p.EnvRules[0].Environment != "production" {
		t.Errorf("names should be stored lowercased: %+v %+v", p.Links[0], p.EnvRules[0])
	}
	if tgt, ok := p.TargetFor("ACME-ORG", "Shop", "Production"); !ok || tgt.ServerID != "srv" {
		t.Errorf("TargetFor = %+v %v", tgt, ok)
	}

	// Deleting the client takes its links and rules with it.
	must(s.DeleteClient("acme"))
	p, _ = s.Policy()
	if len(p.Links) != 0 || len(p.EnvRules) != 0 {
		t.Errorf("client delete did not cascade: %+v", p)
	}
	// Deleting a server takes rules pointing at it with it.
	must(servers.Delete("srv"))
	p, _ = s.Policy()
	if len(p.RepoRules) != 0 {
		t.Errorf("server delete did not cascade to repo rules: %+v", p.RepoRules)
	}
}

// Removal survives a fresh sign-in (upsert keeps revoked_at) and ends every
// session; deploy sources round-trip lowercased and idempotently.
func TestAccessSQL_RemovalAndSources(t *testing.T) {
	s := NewAccessSQL(testDB(t))

	u, err := s.UpsertUser(usecase.User{GitHubID: 7, Login: "contractor", LastLoginAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range [][]byte{[]byte("a"), []byte("b")} {
		if err := s.CreateSession(h, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now()
	if err := s.SetRevoked(u.ID, &at); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserSessions(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser([]byte("a"), time.Now()); err == nil {
		t.Error("session survived DeleteUserSessions")
	}
	again, err := s.UpsertUser(usecase.User{GitHubID: 7, Login: "contractor", LastLoginAt: time.Now()})
	if err != nil || again.RevokedAt == nil {
		t.Fatalf("re-sign-in cleared removal: %+v, %v", again, err)
	}
	if err := s.SetRevoked(u.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(u.ID); got.RevokedAt != nil {
		t.Errorf("restore kept revoked_at: %+v", got)
	}
	if err := s.SetRevoked(999999, nil); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetRevoked(unknown) = %v, want ErrUserNotFound", err)
	}

	for range 2 {
		if err := s.SaveSource(usecase.Source{Account: "Acme-Org", Pattern: "Shop-*"}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sources) != 1 || p.Sources[0] != (usecase.Source{Account: "acme-org", Pattern: "shop-*"}) {
		t.Fatalf("sources = %+v, want one acme-org/shop-*", p.Sources)
	}
	if err := s.DeleteSource("ACME-ORG", "shop-*"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Policy(); len(p.Sources) != 0 {
		t.Errorf("sources after delete = %+v", p.Sources)
	}
}
