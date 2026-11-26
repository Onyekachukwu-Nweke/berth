package usecase

import (
	"context"
	"errors"
	"testing"
)

type fakeInspector struct {
	ports  []int
	err    error
	target SSHTarget
}

func (f *fakeInspector) ListeningPorts(_ context.Context, t SSHTarget) ([]int, error) {
	f.target = t
	return f.ports, f.err
}

func TestServerPortService_ListsViaBerthKey(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "5.6.7.8", SSHPort: 22, Status: StatusReady, Fingerprint: "fp"})
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY-PEM")
	insp := &fakeInspector{ports: []int{22, 80, 443}}
	svc := NewServerPortService(store, vault, insp, nil)

	got, err := svc.Listening(context.Background(), "s1")
	if err != nil {
		t.Fatalf("Listening: %v", err)
	}
	if len(got) != 3 || got[1] != 80 {
		t.Errorf("ports = %v, want [22 80 443]", got)
	}
	// Connects as the berth user with the stored key and pins the host key.
	if insp.target.User != BootstrapUser || insp.target.PrivateKey != "KEY-PEM" || insp.target.Fingerprint != "fp" {
		t.Errorf("unexpected target %+v", insp.target)
	}
}

func TestServerPortService_RequiresReadyServer(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", Status: StatusProbed})
	svc := NewServerPortService(store, vault, &fakeInspector{}, nil)

	if _, err := svc.Listening(context.Background(), "s1"); err == nil {
		t.Fatal("expected an error for a server that is not ready")
	}
}

func TestServerPortService_NotFound(t *testing.T) {
	svc := NewServerPortService(newMemServerStore(), newFakeVault(), &fakeInspector{}, nil)
	if _, err := svc.Listening(context.Background(), "missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("want ErrServerNotFound, got %v", err)
	}
}

// A deploy form must not treat its own running port as taken (a redeploy keeps
// it), and must avoid ports other deployments on the server have claimed even
// when they aren't listening right now.
func TestPorts_SeparatesOwnPortFromOthers(t *testing.T) {
	store, vault := newMemServerStore(), newFakeVault()
	_ = store.Save(Server{ID: "s1", IP: "1.2.3.4", SSHPort: 22, Status: StatusReady})
	_ = vault.SaveSecret(privateKeyKey("s1"), "KEY")
	deps := &fakeDeploymentStore{saved: []Deployment{
		{Owner: "acme", Repo: "shop", Environment: "development", ServerID: "s1", Port: 8010},
		{Owner: "acme", Repo: "api", Environment: "development", ServerID: "s1", Port: 4000},
		{Owner: "acme", Repo: "web", Environment: "development", ServerID: "other", Port: 5000},
	}}
	svc := NewServerPortService(store, vault, &fakeInspector{ports: []int{22, 80, 8010, 4000}}, deps)

	view, err := svc.Ports(context.Background(), "s1", "acme", "shop", "development")
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if view.Current != 8010 {
		t.Errorf("current = %d, want 8010", view.Current)
	}
	for _, p := range view.Listening {
		if p == 8010 {
			t.Error("the deployment's own port must not count as listening")
		}
	}
	if len(view.Reserved) != 1 || view.Reserved[0] != 4000 {
		t.Errorf("reserved = %v, want [4000] (another deployment on this server only)", view.Reserved)
	}
}
