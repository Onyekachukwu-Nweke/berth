package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/goodylili/mountabo/internal/usecase"
)

// AdminHandler manages the placement policy: clients, server ownership, repo
// links and environment rules; and shows users and App installations.
type AdminHandler struct {
	access   *usecase.AccessService
	auth     *usecase.AuthService
	installs usecase.InstallationLister
	log      *slog.Logger
}

// NewAdminHandler builds the admin endpoints.
func NewAdminHandler(access *usecase.AccessService, auth *usecase.AuthService, installs usecase.InstallationLister, log *slog.Logger) *AdminHandler {
	return &AdminHandler{access: access, auth: auth, installs: installs, log: log}
}

func (h *AdminHandler) done(w nethttp.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(nethttp.StatusNoContent)
	case usecase.IsForbidden(err):
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, err.Error())
	case errors.Is(err, usecase.ErrServerNotFound):
		writeErrorTo(w, h.log, nethttp.StatusNotFound, "server not found")
	default:
		h.log.Error("admin change failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusInternalServerError, "could not save the change")
	}
}

type policyResponse struct {
	Clients   []usecase.Client   `json:"clients"`
	Links     []usecase.RepoLink `json:"links"`
	EnvRules  []usecase.EnvRule  `json:"envRules"`
	RepoRules []usecase.RepoRule `json:"repoRules"`
}

// Policy returns the whole placement policy.
func (h *AdminHandler) Policy(w nethttp.ResponseWriter, _ *nethttp.Request) {
	p, err := h.access.Policy()
	if err != nil {
		h.log.Error("read policy failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusInternalServerError, "could not read policy")
		return
	}
	out := policyResponse{Clients: p.Clients, Links: p.Links, EnvRules: p.EnvRules, RepoRules: p.RepoRules}
	if out.Clients == nil {
		out.Clients = []usecase.Client{}
	}
	if out.Links == nil {
		out.Links = []usecase.RepoLink{}
	}
	if out.EnvRules == nil {
		out.EnvRules = []usecase.EnvRule{}
	}
	if out.RepoRules == nil {
		out.RepoRules = []usecase.RepoRule{}
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, out)
}

// SaveClient creates or renames a client: PUT /api/admin/clients/{id} {name}.
func (h *AdminHandler) SaveClient(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	h.done(w, h.access.SaveClient(usecase.Client{ID: r.PathValue("id"), Name: req.Name}))
}

// DeleteClient removes a client.
func (h *AdminHandler) DeleteClient(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.done(w, h.access.DeleteClient(r.PathValue("id")))
}

// SaveLink assigns a repo to a client: PUT /api/admin/links {owner, repo, clientId}.
func (h *AdminHandler) SaveLink(w nethttp.ResponseWriter, r *nethttp.Request) {
	var l usecase.RepoLink
	if err := decodeJSON(w, r, &l); err != nil {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	h.done(w, h.access.SaveLink(l))
}

// DeleteLink unlinks a repo.
func (h *AdminHandler) DeleteLink(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.done(w, h.access.DeleteLink(r.PathValue("owner"), r.PathValue("repo")))
}

// SaveEnvRule points a client environment at a server: PUT /api/admin/clients/{id}/environments/{environment} {serverId}.
func (h *AdminHandler) SaveEnvRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		ServerID string `json:"serverId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	h.done(w, h.access.SaveEnvRule(usecase.EnvRule{
		ClientID: r.PathValue("id"), Environment: r.PathValue("environment"),
		ServerID: req.ServerID,
	}))
}

// DeleteEnvRule removes a client's environment rule.
func (h *AdminHandler) DeleteEnvRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.done(w, h.access.DeleteEnvRule(r.PathValue("id"), r.PathValue("environment")))
}

// SaveRepoRule overrides one repo environment: PUT /api/admin/repo-rules {owner, repo, environment, serverId}.
func (h *AdminHandler) SaveRepoRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	var rule usecase.RepoRule
	if err := decodeJSON(w, r, &rule); err != nil {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	h.done(w, h.access.SaveRepoRule(rule))
}

// DeleteRepoRule removes a repo override.
func (h *AdminHandler) DeleteRepoRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.done(w, h.access.DeleteRepoRule(r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")))
}

// SetServerClient hands a server to a client: PUT /api/admin/servers/{id}/client {clientId} ("" = agency pool).
func (h *AdminHandler) SetServerClient(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		ClientID string `json:"clientId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	h.done(w, h.access.SetServerClient(r.PathValue("id"), req.ClientID))
}

// Users lists everyone who has signed in.
func (h *AdminHandler) Users(w nethttp.ResponseWriter, _ *nethttp.Request) {
	users, err := h.auth.Users()
	if err != nil {
		h.log.Error("list users failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusInternalServerError, "could not list users")
		return
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, users)
}

// Installations lists the accounts the GitHub App is installed on.
func (h *AdminHandler) Installations(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.installs.Installations(r.Context())
	if err != nil {
		h.log.Error("list installations failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusBadGateway, "could not read GitHub App installations")
		return
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, list)
}
