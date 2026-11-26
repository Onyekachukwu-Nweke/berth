package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/goodylili/mountabo/internal/usecase"
)

// AuthHandler serves sign-in, the current user, and the deploy-target lookup.
type AuthHandler struct {
	auth     *usecase.AuthService
	access   *usecase.AccessService
	clientID string
	appSlug  string
	log      *slog.Logger
}

// NewAuthHandler builds the auth endpoints. clientID and appSlug are public:
// the web app needs them to start sign-in and to link to the App's install page.
func NewAuthHandler(auth *usecase.AuthService, access *usecase.AccessService, clientID, appSlug string, log *slog.Logger) *AuthHandler {
	return &AuthHandler{auth: auth, access: access, clientID: clientID, appSlug: appSlug, log: log}
}

// Config returns the public sign-in settings.
func (h *AuthHandler) Config(w nethttp.ResponseWriter, _ *nethttp.Request) {
	install := ""
	if h.appSlug != "" {
		install = "https://github.com/apps/" + h.appSlug + "/installations/new"
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, map[string]string{"clientId": h.clientID, "appSlug": h.appSlug, "installUrl": install})
}

// Login completes GitHub sign-in and returns a session for the web app to keep
// in an httpOnly cookie. The token never reaches browser JavaScript.
func (h *AuthHandler) Login(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirectUri"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.Code == "" {
		writeErrorTo(w, h.log, nethttp.StatusBadRequest, "missing authorization code")
		return
	}
	session, err := h.auth.Login(r.Context(), req.Code, req.RedirectURI)
	if errors.Is(err, usecase.ErrNoAccess) {
		writeErrorTo(w, h.log, nethttp.StatusForbidden, "no_access")
		return
	}
	if errors.Is(err, usecase.ErrRevoked) {
		writeErrorTo(w, h.log, nethttp.StatusForbidden, "removed")
		return
	}
	if err != nil {
		h.log.Error("sign-in failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusBadGateway, "could not complete sign-in with GitHub")
		return
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, session)
}

// Me returns the signed-in user.
func (h *AuthHandler) Me(w nethttp.ResponseWriter, r *nethttp.Request) {
	u, _ := usecase.UserFrom(r.Context())
	writeJSONTo(w, h.log, nethttp.StatusOK, u)
}

// Logout ends the caller's session.
func (h *AuthHandler) Logout(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.auth.Logout(bearer(r)); err != nil {
		h.log.Error("logout failed", "err", err)
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// Targets resolves where each configured environment of a repo deploys, so the
// deploy form can offer environments instead of servers.
func (h *AuthHandler) Targets(w nethttp.ResponseWriter, r *nethttp.Request) {
	u, _ := usecase.UserFrom(r.Context())
	view, err := h.access.Targets(u, r.PathValue("owner"), r.PathValue("repo"))
	if usecase.IsForbidden(err) {
		writeErrorTo(w, h.log, nethttp.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		h.log.Error("resolve targets failed", "err", err)
		writeErrorTo(w, h.log, nethttp.StatusInternalServerError, "could not resolve targets")
		return
	}
	if view.Targets == nil {
		view.Targets = []usecase.Target{}
	}
	writeJSONTo(w, h.log, nethttp.StatusOK, view)
}
