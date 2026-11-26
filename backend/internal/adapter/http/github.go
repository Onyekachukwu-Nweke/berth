package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// maxBodyBytes caps request bodies. Exchange payloads are a short code plus a
// redirect URI, so this is generous while still bounding memory use.
const maxBodyBytes = 64 << 10

// GitHubHandler serves the GitHub connection endpoints over the usecase layer.
type GitHubHandler struct {
	connector  *usecase.GitHubConnector
	tree       *usecase.TreeService
	envExample *usecase.EnvExampleService
	runSteps   *usecase.RunStepsService
	branches   *usecase.BranchesService
	files      *usecase.RepoFileService
	log        *slog.Logger
}

// NewGitHubHandler wires the handler to the connector, the repo-tree service,
// the env-example service, the run-steps service, the branches service, and a
// logger.
func NewGitHubHandler(connector *usecase.GitHubConnector, tree *usecase.TreeService, envExample *usecase.EnvExampleService, runSteps *usecase.RunStepsService, branches *usecase.BranchesService, files *usecase.RepoFileService, log *slog.Logger) *GitHubHandler {
	return &GitHubHandler{connector: connector, tree: tree, envExample: envExample, runSteps: runSteps, branches: branches, files: files, log: log}
}

// CommitFiles writes a few files (a Docker setup from a template) onto a
// branch: POST /api/repos/{owner}/{repo}/files {branch, message, files:[{path, content}]}.
// Push access is enforced by the route's guard.
func (h *GitHubHandler) CommitFiles(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		Branch  string             `json:"branch"`
		Message string             `json:"message"`
		Files   []usecase.RepoFile `json:"files"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		h.writeError(w, nethttp.StatusBadRequest, "invalid request body")
		return
	}
	err := h.files.Commit(r.Context(), usecase.CommitFilesInput{
		Owner: r.PathValue("owner"), Repo: r.PathValue("repo"),
		Branch: req.Branch, Message: req.Message, Files: req.Files,
	})
	switch {
	case err == nil:
		h.writeJSON(w, nethttp.StatusOK, map[string]int{"committed": len(req.Files)})
	case errors.Is(err, usecase.ErrInvalidFiles):
		h.writeError(w, nethttp.StatusBadRequest, err.Error())
	case errors.Is(err, usecase.ErrNotConnected):
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
	default:
		h.log.Error("commit files failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "GitHub refused the commit; check that the branch exists and isn't protected")
	}
}

// Branches lists every branch on owner/repo so the new-environment picker can
// present the real branch list instead of a free-text field. owner and repo
// are required; not-connected is reported as 401.
func (h *GitHubHandler) Branches(w nethttp.ResponseWriter, r *nethttp.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if owner == "" || repo == "" {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner or repo")
		return
	}
	names, err := h.branches.List(r.Context(), owner, repo)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("list branches failed", "owner", owner, "repo", repo, "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not list repository branches")
		return
	}
	h.writeJSON(w, nethttp.StatusOK, map[string][]string{"branches": names})
}

type repoResponse struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"fullName"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"defaultBranch"`
	Language      string `json:"language"`
	PushedAt      string `json:"pushedAt"`
	HasDocker     bool   `json:"hasDocker"`
	Kind          string `json:"kind"`       // "compose" | "docker" | "none"
	Permission    string `json:"permission"` // "read" | "write" | "admin"
}

// Repos lists the repos the signed-in person can reach through the App, with
// their permission on each, so the UI shows exactly what GitHub allows them.
func (h *GitHubHandler) Repos(w nethttp.ResponseWriter, r *nethttp.Request) {
	u, _ := usecase.UserFrom(r.Context())
	repos, err := h.connector.Repositories(r.Context(), u)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("list repos failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not list repositories")
		return
	}

	out := make([]repoResponse, 0, len(repos))
	for _, rp := range repos {
		pushed := ""
		if !rp.PushedAt.IsZero() {
			pushed = rp.PushedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, repoResponse{
			Owner:         rp.Owner,
			Name:          rp.Name,
			FullName:      rp.FullName,
			Private:       rp.Private,
			DefaultBranch: rp.DefaultBranch,
			Language:      rp.Language,
			PushedAt:      pushed,
			HasDocker:     rp.HasDocker,
			Kind:          rp.Kind,
			Permission:    rp.Permission.String(),
		})
	}
	h.writeJSON(w, nethttp.StatusOK, out)
}

type portResponse struct {
	Service   string `json:"service"`
	EnvVar    string `json:"envVar"`
	Host      string `json:"host"`
	Container string `json:"container"`
	Editable  bool   `json:"editable"`
}

type portsResponse struct {
	Strategy string         `json:"strategy"` // "compose", "docker", or "" when neither
	Ports    []portResponse `json:"ports"`
}

// Ports reports the published ports declared in a repo's container config and
// the deploy strategy that fits, so the configure UI can offer the project's
// real ports and generate the right deploy. owner and repo are required; ref
// (branch/sha) and dir (sub-directory) are optional. A repo with no detectable
// ports returns an empty array and strategy "", not an error.
func (h *GitHubHandler) Ports(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	owner, repo := q.Get("owner"), q.Get("repo")
	if owner == "" || repo == "" {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner or repo")
		return
	}

	ref := usecase.RepoRef{Owner: owner, Name: repo, Ref: q.Get("ref"), Dir: q.Get("dir")}
	ports, strategy, err := h.connector.DetectPorts(r.Context(), ref)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("detect ports failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not detect ports")
		return
	}

	out := portsResponse{Strategy: string(strategy), Ports: make([]portResponse, 0, len(ports))}
	for _, p := range ports {
		out.Ports = append(out.Ports, portResponse{
			Service:   p.Service,
			EnvVar:    p.EnvVar,
			Host:      p.Host,
			Container: p.Container,
			Editable:  p.Editable,
		})
	}
	h.writeJSON(w, nethttp.StatusOK, out)
}

// Tree lists every path in a repo at a ref so the configure UI can offer a
// directory/file picker instead of a free-text path. owner, repo and ref are
// all required. Not-connected is reported as 401.
func (h *GitHubHandler) Tree(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	owner, repo, ref := q.Get("owner"), q.Get("repo"), q.Get("ref")
	if owner == "" || repo == "" || ref == "" {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner, repo or ref")
		return
	}

	entries, err := h.tree.Tree(r.Context(), owner, repo, ref)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("list tree failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not list repository tree")
		return
	}
	h.writeJSON(w, nethttp.StatusOK, entries)
}

// EnvExample reports the variable names declared in a repo's example env file
// (.env.example or a common variant), so the configure UI can pre-fill the env
// var rows for the operator to fill in. owner and repo are required; ref
// (branch/sha) and dir (sub-directory) are optional. A repo with no example file
// returns an empty array, not an error.
func (h *GitHubHandler) EnvExample(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	owner, repo := q.Get("owner"), q.Get("repo")
	if owner == "" || repo == "" {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner or repo")
		return
	}

	ref := usecase.RepoRef{Owner: owner, Name: repo, Ref: q.Get("ref"), Dir: q.Get("dir")}
	keys, err := h.envExample.Keys(r.Context(), ref)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("read env example failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not read the example env file")
		return
	}

	out := make([]string, 0, len(keys))
	out = append(out, keys...)
	h.writeJSON(w, nethttp.StatusOK, out)
}

// RunSteps reports the latest deploy run's job and step progress for owner/repo
// on a branch, so the UI can show each GitHub Actions step's live status. owner
// and repo are required; ref is the branch (defaulting to "main"). A workflow
// with no run yet returns an empty result, not an error. Not-connected is
// reported as 401.
func (h *GitHubHandler) RunSteps(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	owner, repo := q.Get("owner"), q.Get("repo")
	if owner == "" || repo == "" {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner or repo")
		return
	}
	ref := q.Get("ref")
	if ref == "" {
		ref = "main"
	}

	steps, err := h.runSteps.Steps(r.Context(), owner, repo, ref, q.Get("environment"))
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("read run steps failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not read the deploy run steps")
		return
	}
	if steps.Jobs == nil {
		steps.Jobs = []usecase.RunJob{}
	}
	h.writeJSON(w, nethttp.StatusOK, steps)
}

// JobLogs returns one job's plain-text log (split into lines) from the latest
// deploy run, so the walkthrough can show what each step printed and what
// failed. owner, repo and jobId are required. Not-connected is reported as 401.
func (h *GitHubHandler) JobLogs(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	owner, repo := q.Get("owner"), q.Get("repo")
	jobID, _ := strconv.ParseInt(q.Get("jobId"), 10, 64)
	if owner == "" || repo == "" || jobID == 0 {
		h.writeError(w, nethttp.StatusBadRequest, "missing owner, repo or jobId")
		return
	}

	lines, err := h.runSteps.JobLog(r.Context(), owner, repo, jobID)
	if errors.Is(err, usecase.ErrNotConnected) {
		h.writeError(w, nethttp.StatusFailedDependency, "the berth GitHub App is not installed on this repository's account")
		return
	}
	if err != nil {
		h.log.Error("read job logs failed", "err", err)
		h.writeError(w, nethttp.StatusBadGateway, "could not read the job logs")
		return
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines...)
	h.writeJSON(w, nethttp.StatusOK, map[string][]string{"lines": out})
}

// decodeJSON reads a single JSON object from the request body, bounding its size
// and rejecting unknown fields so malformed callers fail loudly.
func decodeJSON(w nethttp.ResponseWriter, r *nethttp.Request, dst any) error {
	r.Body = nethttp.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func (h *GitHubHandler) writeJSON(w nethttp.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The response is already committed by WriteHeader, so an encode failure can
	// only be logged, not recovered from.
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		h.log.Error("encode response", "err", err)
	}
}

func (h *GitHubHandler) writeError(w nethttp.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, map[string]string{"error": message})
}
