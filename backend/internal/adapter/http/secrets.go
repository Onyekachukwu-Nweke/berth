package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/goodylili/mountabo/internal/usecase"
)

// SecretsHandler serves a deployment environment's Actions secrets: listing the
// configured names (from GitHub) with mountabo's revealable values, setting new
// values, and removing them. Values are a sensitive read surface, so responses
// are marked no-store and request/response bodies are never logged.
type SecretsHandler struct {
	svc *usecase.SecretsService
	log *slog.Logger
}

// NewSecretsHandler wires the handler to the secrets service.
func NewSecretsHandler(svc *usecase.SecretsService, log *slog.Logger) *SecretsHandler {
	return &SecretsHandler{svc: svc, log: log}
}

// List returns the environment's configured secrets (names, managed flag,
// updated-at, status) and the .env.example variables not yet set. Values are
// included only with ?includeValues=true (the compare view omits it). 401 when
// not connected.
func (h *SecretsHandler) List(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, environment := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	includeValues := r.URL.Query().Get("includeValues") == "true"
	view, err := h.svc.List(r.Context(), owner, repo, environment, includeValues)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusUnauthorized, "github not connected")
		return
	}
	if err != nil {
		h.log.Error("list env secrets failed", "repo", owner+"/"+repo, "environment", environment, "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not list secrets")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(nethttp.StatusOK)
	if err := json.NewEncoder(w).Encode(view); err != nil {
		h.log.Error("encode env secrets", "err", err)
	}
}

// Set writes operator secrets to the environment (and records their values
// locally). A managed name is 400; a partial sync (set on GitHub but not stored
// locally) is a 200 whose body flags `partial:true` so the UI warns rather than
// reporting plain success. The request body (secret values) is never logged.
func (h *SecretsHandler) Set(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, environment := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	var body struct {
		Secrets []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"secrets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	secrets := make([]usecase.NamedSecret, 0, len(body.Secrets))
	for _, s := range body.Secrets {
		if s.Key == "" {
			h.writeError(w, nethttp.StatusBadRequest, "each secret needs a name")
			return
		}
		secrets = append(secrets, usecase.NamedSecret{Name: s.Key, Value: s.Value})
	}

	result, err := h.svc.Set(r.Context(), owner, repo, environment, secrets)
	switch {
	case errors.Is(err, usecase.ErrManagedSecret):
		h.writeError(w, nethttp.StatusBadRequest, "that secret is managed by mountabo and can't be edited")
		return
	case errors.Is(err, usecase.ErrNotConnected):
		h.writeError(w, nethttp.StatusUnauthorized, "github not connected")
		return
	case err != nil:
		h.log.Error("set env secrets failed", "repo", owner+"/"+repo, "environment", environment, "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not set secrets")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(nethttp.StatusOK)
	if err := json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
		usecase.SetResult
	}{OK: !result.Partial, SetResult: result}); err != nil {
		h.log.Error("encode set-secrets result", "err", err)
	}
}

// Delete removes one operator secret from the environment (GitHub + local value).
// A managed name is 400.
func (h *SecretsHandler) Delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, environment, name := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment"), r.PathValue("name")
	err := h.svc.Delete(r.Context(), owner, repo, environment, name)
	switch {
	case errors.Is(err, usecase.ErrManagedSecret):
		h.writeError(w, nethttp.StatusBadRequest, "that secret is managed by mountabo and can't be deleted")
		return
	case errors.Is(err, usecase.ErrNotConnected):
		h.writeError(w, nethttp.StatusUnauthorized, "github not connected")
		return
	case err != nil:
		h.log.Error("delete env secret failed", "repo", owner+"/"+repo, "environment", environment, "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not delete the secret")
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *SecretsHandler) writeError(w nethttp.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		h.log.Error("encode response", "err", err)
	}
}
