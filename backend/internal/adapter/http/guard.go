package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"strings"

	"github.com/goodylili/mountabo/internal/usecase"
)

// Guard puts every route behind a decision. The API is only ever called by the
// web app's server, which forwards the person's session as a bearer token; the
// guard resolves it to a user and applies the route's rule before the handler
// runs, so no handler has to remember to check.
type Guard struct {
	auth   *usecase.AuthService
	access *usecase.AccessService
	log    *slog.Logger
}

// NewGuard builds the route guard.
func NewGuard(auth *usecase.AuthService, access *usecase.AccessService, log *slog.Logger) *Guard {
	return &Guard{auth: auth, access: access, log: log}
}

func bearer(r *nethttp.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func writeJSONTo(w nethttp.ResponseWriter, log *slog.Logger, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Error("encode response", "err", err)
	}
}

func writeErrorTo(w nethttp.ResponseWriter, log *slog.Logger, status int, message string) {
	writeJSONTo(w, log, status, map[string]string{"error": message})
}

// deny maps an access decision error onto a response.
func (g *Guard) deny(w nethttp.ResponseWriter, err error) {
	switch {
	case usecase.IsForbidden(err):
		writeErrorTo(w, g.log, nethttp.StatusForbidden, err.Error())
	case errors.Is(err, usecase.ErrServerNotFound):
		writeErrorTo(w, g.log, nethttp.StatusNotFound, "server not found")
	default:
		g.log.Error("access check failed", "err", err)
		writeErrorTo(w, g.log, nethttp.StatusInternalServerError, "could not check access")
	}
}

// User requires a signed-in user and carries them in the request context.
func (g *Guard) User(h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		u, err := g.auth.Authenticate(r.Context(), bearer(r))
		if err != nil {
			writeErrorTo(w, g.log, nethttp.StatusUnauthorized, "sign in required")
			return
		}
		h(w, r.WithContext(usecase.WithUser(r.Context(), u)))
	}
}

// Admin requires an admin.
func (g *Guard) Admin(h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return g.User(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if u, _ := usecase.UserFrom(r.Context()); !u.IsAdmin {
			writeErrorTo(w, g.log, nethttp.StatusForbidden, "admins only")
			return
		}
		h(w, r)
	})
}

// repoOf reads owner/repo from the path, falling back to the query string.
func repoOf(r *nethttp.Request) (string, string) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if owner == "" {
		owner = r.URL.Query().Get("owner")
	}
	if repo == "" {
		repo = r.URL.Query().Get("repo")
	}
	return owner, repo
}

// Repo requires at least want on the route's repo (from the path or query).
func (g *Guard) Repo(want usecase.Permission, h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return g.User(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		owner, repo := repoOf(r)
		if owner == "" || repo == "" {
			writeErrorTo(w, g.log, nethttp.StatusBadRequest, "missing owner or repo")
			return
		}
		u, _ := usecase.UserFrom(r.Context())
		if err := g.access.RequireRepo(u, owner, repo, want); err != nil {
			g.deny(w, err)
			return
		}
		h(w, r)
	})
}

// Change guards direct changes to a live environment ({owner}/{repo}/{environment}):
// write access, and admin-only where the environment requires approval.
func (g *Guard) Change(h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return g.User(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		u, _ := usecase.UserFrom(r.Context())
		if err := g.access.RequireChange(u, r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")); err != nil {
			g.deny(w, err)
			return
		}
		h(w, r)
	})
}

// Server requires that the route's server ({id}) is one the user has a reason
// to see: a target of one of their repos, or running one.
func (g *Guard) Server(h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return g.User(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		u, _ := usecase.UserFrom(r.Context())
		ok, err := g.access.CanSeeServer(u, r.PathValue("id"))
		if err != nil {
			g.deny(w, err)
			return
		}
		if !ok {
			writeErrorTo(w, g.log, nethttp.StatusForbidden, "you don't have access to this server")
			return
		}
		h(w, r)
	})
}

// Deploy checks a deploy request against the placement policy before the
// handler sees it: write access to the repo, client isolation, and (for
// developers) that the environment's rule points at this server.
func (g *Guard) Deploy(h nethttp.HandlerFunc) nethttp.HandlerFunc {
	return g.User(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, err := io.ReadAll(nethttp.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeErrorTo(w, g.log, nethttp.StatusBadRequest, "invalid request body")
			return
		}
		var target struct {
			Owner       string `json:"owner"`
			Repo        string `json:"repo"`
			Branch      string `json:"branch"`
			Environment string `json:"environment"`
		}
		if err := json.Unmarshal(body, &target); err != nil {
			writeErrorTo(w, g.log, nethttp.StatusBadRequest, "invalid request body")
			return
		}
		// Same defaulting as the deploy itself: no environment means the branch.
		env := strings.TrimSpace(target.Environment)
		if env == "" {
			env = strings.TrimSpace(target.Branch)
		}
		u, _ := usecase.UserFrom(r.Context())
		if err := g.access.CheckDeploy(u, target.Owner, target.Repo, env, r.PathValue("id")); err != nil {
			g.deny(w, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	})
}
