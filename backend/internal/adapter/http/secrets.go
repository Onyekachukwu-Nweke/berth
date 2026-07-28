package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/goodylili/mountabo/internal/usecase"
)

// SecretsHandler serves a deployment environment's GitHub Actions secrets:
// listing the configured names (from GitHub), setting values (write-only), and
// removing them. GitHub secret values can't be read back, so nothing here
// exposes a value; responses are still marked no-store and bodies never logged.
type SecretsHandler struct {
	svc *usecase.SecretsService
	log *slog.Logger
}

// NewSecretsHandler wires the handler to the secrets service.
func NewSecretsHandler(svc *usecase.SecretsService, log *slog.Logger) *SecretsHandler {
	return &SecretsHandler{svc: svc, log: log}
}

// List returns the environment's configured secrets (names, managed flag,
// updated-at) and the .env.example variables not yet set. 401 when not connected.
func (h *SecretsHandler) List(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, environment := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	view, err := h.svc.List(r.Context(), owner, repo, environment)
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

// Set writes operator secrets to the GitHub environment (write-only). A managed
// or invalid name is 400; success is 204 No Content (there is no value/status to
// return). The request body (secret values) is never logged.
func (h *SecretsHandler) Set(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, environment := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	var body struct {
		Secrets []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"secrets"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
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

	err := h.svc.Set(r.Context(), owner, repo, environment, secrets)
	switch {
	case errors.Is(err, usecase.ErrManagedSecret):
		h.writeError(w, nethttp.StatusBadRequest, "that secret is managed by mountabo and can't be edited")
		return
	case errors.Is(err, usecase.ErrInvalidSecretName):
		h.writeError(w, nethttp.StatusBadRequest, "secret names must be letters, digits, or underscores and can't start with a digit")
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
	w.WriteHeader(nethttp.StatusNoContent)
}

// Delete removes one operator secret from the GitHub environment. A managed name
// is 400.
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
