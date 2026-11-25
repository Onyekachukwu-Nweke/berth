package usecase

import (
	"context"
	"fmt"
	"strings"
)

// PortInspector lists the ports currently in a listening state on a server,
// connecting over SSH. It only reads; it never changes anything.
type PortInspector interface {
	ListeningPorts(ctx context.Context, t SSHTarget) ([]int, error)
}

// ServerPortService reports which ports are already occupied on a set-up
// server, so the UI can flag (never auto-fix) a deploy port that would collide.
type ServerPortService struct {
	servers     ServerStore
	vault       SecretVault
	inspector   PortInspector
	deployments DeploymentStore
}

// NewServerPortService wires the service to its ports. deployments tells it
// which ports mountabo's own deployments on a server have claimed.
func NewServerPortService(servers ServerStore, vault SecretVault, inspector PortInspector, deployments DeploymentStore) *ServerPortService {
	return &ServerPortService{servers: servers, vault: vault, inspector: inspector, deployments: deployments}
}

// PortsView is what a deploy form needs to pick a host port without a
// collision: what is listening on the server now, the ports mountabo's other
// deployments there have claimed (even if stopped right now), and this
// deployment's own current port, which is not a collision on a redeploy.
type PortsView struct {
	Listening []int `json:"listening"`
	Reserved  []int `json:"reserved"`
	Current   int   `json:"current,omitempty"`
}

// Ports reports the server's ports for deploying owner/repo's environment.
// The deployment's own current port is left out of listening and reserved.
func (s *ServerPortService) Ports(ctx context.Context, id, owner, repo, environment string) (PortsView, error) {
	listening, err := s.Listening(ctx, id)
	if err != nil {
		return PortsView{}, err
	}
	view := PortsView{Listening: listening, Reserved: []int{}}
	if s.deployments == nil {
		return view, nil
	}
	deps, err := s.deployments.List()
	if err != nil {
		return view, nil
	}
	for _, d := range deps {
		if d.ServerID != id || d.Port <= 0 {
			continue
		}
		mine := owner != "" && strings.EqualFold(d.Owner, owner) && strings.EqualFold(d.Repo, repo) && strings.EqualFold(d.Environment, environment)
		if mine {
			view.Current = d.Port
			continue
		}
		view.Reserved = append(view.Reserved, d.Port)
	}
	if view.Current > 0 {
		kept := view.Listening[:0:0]
		for _, p := range view.Listening {
			if p != view.Current {
				kept = append(kept, p)
			}
		}
		view.Listening = kept
	}
	return view, nil
}

// Listening returns the ports already bound on the server, connecting as the
// mountabo user with its stored key. The server must be set up (the key
// exists). ErrServerNotFound propagates from the store.
func (s *ServerPortService) Listening(ctx context.Context, id string) ([]int, error) {
	server, err := s.servers.Get(id)
	if err != nil {
		return nil, err
	}
	if server.Status != StatusReady {
		return nil, fmt.Errorf("server must be set up before checking ports")
	}

	key, err := s.vault.LoadSecret(privateKeyKey(id))
	if err != nil {
		return nil, fmt.Errorf("load server key: %w", err)
	}

	target := SSHTarget{Host: server.IP, Port: server.SSHPort, User: BootstrapUser, PrivateKey: key, Fingerprint: server.Fingerprint}
	ports, err := s.inspector.ListeningPorts(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("inspect ports: %w", err)
	}
	return ports, nil
}
