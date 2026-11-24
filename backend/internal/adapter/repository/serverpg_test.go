package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

func TestServerSQL_RoundTrip(t *testing.T) {
	store := NewServerSQL(testDB(t))

	if _, err := store.Get("nope"); !errors.Is(err, usecase.ErrServerNotFound) {
		t.Fatalf("Get(missing) err = %v, want ErrServerNotFound", err)
	}

	srv := usecase.Server{
		ID: "s1", Name: "prod-1", IP: "10.0.0.1", SSHPort: 22,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Options:   []string{"ufw"},
		Domains:   []usecase.Domain{{Host: "app.example.com", Upstream: "127.0.0.1:3000"}},
	}
	if err := store.Save(srv); err != nil {
		t.Fatalf("Save: %v", err)
	}
	srv.Name = "prod-renamed"
	if err := store.Save(srv); err != nil {
		t.Fatalf("Save update: %v", err)
	}
	if err := store.Save(usecase.Server{ID: "s2", Name: "second"}); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	got, err := store.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "prod-renamed" || len(got.Domains) != 1 || got.Domains[0].Host != "app.example.com" || len(got.Options) != 1 {
		t.Errorf("round-trip lost data: %+v", got)
	}
	list, err := store.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %v err=%v", list, err)
	}

	if err := store.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete("s1"); err != nil {
		t.Fatalf("Delete twice should be a no-op: %v", err)
	}
	if list, _ := store.List(); len(list) != 1 {
		t.Errorf("after delete want 1 server, got %d", len(list))
	}
}
