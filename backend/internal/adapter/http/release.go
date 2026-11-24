package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	nethttp "net/http"
	"strings"

	"github.com/goodylili/mountabo/internal/usecase"
)

// ReleaseHandler exposes a deployment's release state and the two operations that
// re-point it at a retained artifact.
//
// Mutations are POST and return an operation to observe; the log is a separate
// GET stream. Driving a mutation from the SSE GET itself would mean an
// EventSource reconnect — which browsers do automatically on any dropped
// connection — silently re-running the rollback.
type ReleaseHandler struct {
	svc *usecase.RollbackService
	ops *operationStore
	log *slog.Logger
}

// NewReleaseHandler wires the handler to the rollback service.
func NewReleaseHandler(svc *usecase.RollbackService, log *slog.Logger) *ReleaseHandler {
	return &ReleaseHandler{svc: svc, ops: newOperationStore(), log: log}
}

// State returns the deployment's release pointers as the server holds them.
func (h *ReleaseHandler) State(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, env := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	state, err := h.svc.State(r.Context(), owner, repo, env)
	if err != nil {
		h.log.Warn("read release state failed", "repo", owner+"/"+repo, "env", env, "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not read release state from the server")
		return
	}
	h.writeJSON(w, nethttp.StatusOK, state)
}

// rollbackRequest is the POST body for a rollback. An empty artifact means the
// release before the active one.
type rollbackRequest struct {
	Artifact string `json:"artifact"`
	Force    bool   `json:"force"`
}

// Rollback creates a rollback operation and returns its id. The work runs in the
// background; the caller watches it on Logs.
func (h *ReleaseHandler) Rollback(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, env := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	var req rollbackRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	in := usecase.RollbackInput{Owner: owner, Repo: repo, Environment: env, ArtifactID: req.Artifact, Force: req.Force}
	// Deliberately NOT r.Context(): that is cancelled the moment this POST
	// responds, which would kill the rollback it just started. The operation
	// outlives the request that created it — abandoning a half-applied activation
	// is far worse than finishing one nobody is watching.
	id := h.ops.start(func(w2 opWriter) error { return h.svc.Rollback(context.Background(), in, w2) })
	h.writeJSON(w, nethttp.StatusAccepted, map[string]string{"operationId": id})
}

// Redeploy creates a redeploy operation: re-point at the artifact already active.
func (h *ReleaseHandler) Redeploy(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, env := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	// See Rollback: the operation must outlive the request that created it.
	id := h.ops.start(func(w2 opWriter) error { return h.svc.Redeploy(context.Background(), owner, repo, env, w2) })
	h.writeJSON(w, nethttp.StatusAccepted, map[string]string{"operationId": id})
}

// approveRequest authorises one candidate's gated hooks.
type approveRequest struct {
	Artifact     string   `json:"artifact"`
	HooksVersion string   `json:"hooksVersion"`
	HookIDs      []string `json:"hookIds"`
	Actor        string   `json:"actor"`
}

// Approve runs a candidate's gated hooks and continues the deploy that halted
// waiting for them. It is the only path that sets the approval marker: the
// generated workflow never forwards it, so a push cannot run a gated migration.
func (h *ReleaseHandler) Approve(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo, env := r.PathValue("owner"), r.PathValue("repo"), r.PathValue("environment")
	var req approveRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if strings.TrimSpace(req.Artifact) == "" || strings.TrimSpace(req.HooksVersion) == "" {
		h.writeError(w, nethttp.StatusBadRequest, "an approval must name the release and the hook set it was reviewed against")
		return
	}
	in := usecase.ApproveInput{
		Owner: owner, Repo: repo, Environment: env,
		ArtifactID: req.Artifact, HooksVersion: req.HooksVersion,
		HookIDs: req.HookIDs, Actor: req.Actor,
	}
	// See Rollback: the operation must outlive the request that created it.
	id := h.ops.start(func(w2 opWriter) error { return h.svc.ApproveAndContinue(context.Background(), in, w2) })
	h.writeJSON(w, nethttp.StatusAccepted, map[string]string{"operationId": id})
}

// Logs streams one operation's output. It is read-only and replays from the
// start, so a dropped connection can be resumed without repeating the work.
func (h *ReleaseHandler) Logs(w nethttp.ResponseWriter, r *nethttp.Request) {
	id := r.PathValue("id")
	op, ok := h.ops.get(id)
	if !ok {
		h.writeError(w, nethttp.StatusNotFound, "operation not found")
		return
	}
	streamSSE(w, h.log, "done", func(out io.Writer) error { return op.stream(r.Context(), out) })
}

// writeJSON encodes a successful response.
func (h *ReleaseHandler) writeJSON(w nethttp.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log.Error("encode response", "err", err)
	}
}

func (h *ReleaseHandler) writeError(w nethttp.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]string{"error": message})
}
