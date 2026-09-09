package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/goodylili/mountabo/internal/workflow"
)

// ErrDeploymentNotFound is returned when no tracked deployment matches an app.
var ErrDeploymentNotFound = errors.New("deployment not found")

// Deployment is a repo+branch mountabo has configured to deploy to a server. It
// is persisted on a successful deploy so the monitor can show its history.
type Deployment struct {
	ID           string `json:"id"`
	App          string `json:"app"`
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	Environment  string `json:"environment"`
	ServerID     string `json:"serverId"`
	WorkflowFile string `json:"workflowFile"`
	// DeploymentID is the canonical server-side identity every resource this
	// deployment owns is named from: the compose project, containers, the image
	// namespace, the checkout path, the staging directory, the lock, the state
	// document and the ownership labels teardown selects on. Derived once from
	// (owner, repo, environment) by DeriveDeploymentID and persisted, so renaming
	// the app — or a future change to the derivation — can never re-home a live
	// deployment's resources.
	DeploymentID string `json:"deploymentId"`
	// Port is the app's primary published host port on the server (the first
	// configured deploy port), so a health check can probe 127.0.0.1:<port> over
	// SSH. Zero when no port was configured (e.g. a compose stack with no host
	// publish), in which case the health check falls back to the domain.
	Port      int       `json:"port"`
	CreatedAt time.Time `json:"createdAt"`
	// RootDir is where the app's compose/Dockerfile lives in the repo (repo root
	// when empty), persisted so per-environment tooling (e.g. .env.example
	// detection) reads the right directory rather than assuming the root.
	RootDir string `json:"rootDir"`
	// Config is the non-secret deploy configuration persisted so a re-apply can
	// regenerate the workflow + deploy.sh (e.g. after a secret changes) without
	// the operator re-entering it. Nil marks a legacy deployment recorded before
	// this was persisted — it cannot be re-applied from the secrets tab. Kept out
	// of any HTTP payload (json:"-"); the repo layer stores it as a JSON column.
	Config *DeployConfig `json:"-"`
	// CommitSHA and Actor describe the release this deploy created: the branch
	// head being deployed and the GitHub login that triggered it. They are
	// recorded onto the appended release event by DeploymentStore.Save, not
	// persisted on the deployments state row (List does not populate them).
	CommitSHA string `json:"-"`
	Actor     string `json:"-"`
}

// DeploymentStore persists configured deployments. Save upserts by
// owner+repo+environment, so re-deploying an environment updates its record
// rather than duplicating it.
type DeploymentStore interface {
	List() ([]Deployment, error)
	Save(d Deployment) error
}

// DeploymentDeleter forgets a tracked deployment by its identity (owner, repo,
// environment): it removes the deployment record and its append-only event
// history, returning whether a record existed to remove. It only drops
// mountabo's tracking; it never touches the running app or the committed workflow.
type DeploymentDeleter interface {
	Delete(owner, repo, environment string) (removed bool, err error)
}

// DeploymentPortSetter updates a deployment's recorded published port, without
// touching its release history, so a port an operator declares later (e.g. when
// attaching a domain) is recorded and the deployment and its domain agree. It
// reports whether a row was actually updated (false = no such deployment), so a
// row that vanished between lookup and update is not mistaken for success.
type DeploymentPortSetter interface {
	SetPort(owner, repo, environment string, port int) (updated bool, err error)
}

// ContainerTeardown removes the resources one deployment owns on a server over
// SSH — its containers (compose stack or plain container), the images mountabo
// built for it, and its staging directory — so the deployment stops existing.
//
// It is keyed on the deployment id and selects by ownership label, never by
// name: an app name and a branch can be shared by two deployments, so a
// name-based sweep could remove another deployment's containers. It is
// best-effort: docker missing on the box or no matching resource is not an error.
type ContainerTeardown interface {
	RemoveApp(ctx context.Context, t SSHTarget, deploymentID string) error
}

// PurgeSpec describes what one deployment owns on its server.
//
// Every field is something teardown must be able to identify precisely, because
// removing a resource it does not own is the failure mode that matters most.
type PurgeSpec struct {
	// DeploymentID is the canonical identity every resource is labelled with.
	DeploymentID string
	// DeployDir is the root the checkout lives under, used as the containment
	// boundary for filesystem removal.
	DeployDir string
	// DeployKeyFile is the private key installed on the box for this repo.
	DeployKeyFile string
	// RemoveVolumes deletes the project's named volumes. This destroys databases
	// and uploads, so it is opt-in and off by default.
	RemoveVolumes bool
	// RemoveDeployKey is set only when no other environment of the same repo
	// remains — the key is per-repo, not per-deployment.
	RemoveDeployKey bool
}

// AppPurger removes the resources a deployment owns on its server, streaming a
// journal of what it did.
//
// It reports "cleanup incomplete" rather than success when something could not be
// removed: a teardown that says "done" while leaving containers and plaintext
// secrets behind is worse than one that admits what it left.
type AppPurger interface {
	PurgeApp(ctx context.Context, t SSHTarget, spec PurgeSpec, out io.Writer) error
}

// RepoEnvironmentDeleter removes a GitHub deployment environment (and the
// secrets scoped to it, which GitHub drops together) from a repo. An already
// gone environment is a no-op success, so the teardown does not fail when the
// environment was never created or has been removed manually.
type RepoEnvironmentDeleter interface {
	DeleteEnvironment(ctx context.Context, t Token, owner, repo, name string) error
}

// RepoKeyDeleter removes mountabo's deploy keys from a repo. It matches keys by
// title prefix so it only removes the ones mountabo added (the title carries a
// "mountabo deploy" prefix); keys the operator created for other tools are
// untouched. No matching key is a no-op success.
type RepoKeyDeleter interface {
	DeleteDeployKey(ctx context.Context, t Token, owner, repo, titlePrefix string) error
}

// RepoFileDeleter removes a single file from a repo branch via the contents
// API. A file that is already gone is treated as a no-op success, so tearing
// down a deployment never fails on an already-removed file.
type RepoFileDeleter interface {
	DeleteFile(ctx context.Context, t Token, owner, repo, branch, path, message string) error
}

// WorkflowRun is one GitHub Actions run of a deploy workflow.
type WorkflowRun struct {
	SHA        string
	Title      string
	Status     string // queued | in_progress | completed
	Conclusion string // success | failure | ... (set once completed)
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// HTMLURL is the Actions run page on github.com, so the UI can link straight
	// to the run.
	HTMLURL string
}

// WorkflowRunLister lists recent runs of a repo's workflow file on a branch,
// newest first, capped to limit.
type WorkflowRunLister interface {
	ListWorkflowRuns(ctx context.Context, t Token, owner, repo, workflowFile, branch string, limit int) ([]WorkflowRun, error)
}

// RunView is one deploy run shaped for the UI.
type RunView struct {
	SHA       string `json:"sha"`
	Message   string `json:"message"`
	Status    string `json:"status"` // success | failed | running
	When      string `json:"when"`
	Duration  string `json:"duration"`
	RunURL    string `json:"runUrl"`    // the Actions run page on github.com
	CommitURL string `json:"commitUrl"` // the run's commit on github.com
}

// Release is one recorded deploy from the append-only log, enriched into a
// durable release record. CommitSHA and Actor are captured at deploy time;
// Status starts "pending" and is overlaid live from the matching Actions run at
// read time; ImageRef, Health and RunURL are provisioned but populated by later
// phases. PrevReleaseID links to the target's previous release (nil for the
// first), so release history is a chain rollback can walk.
type Release struct {
	ID            int64
	At            time.Time
	Environment   string
	CommitSHA     string
	ImageRef      string
	Status        string
	Health        string
	Actor         string
	RunURL        string
	PrevReleaseID *int64
}

// ReleaseReader reads an environment's release history from the append-only log:
// the most recent releases (newest first, capped to limit) and the total count.
type ReleaseReader interface {
	Releases(owner, repo, environment string, limit int) (releases []Release, total int, err error)
}

// ReleaseView is one release shaped for the UI timeline. The JSON key stays
// "timeline" on the enclosing status for frontend compatibility.
type ReleaseView struct {
	When        string `json:"when"`
	Environment string `json:"environment"`
	SHA         string `json:"sha"`
	Status      string `json:"status"`
	Actor       string `json:"actor"`
	RunURL      string `json:"runUrl"`
}

// DeploymentStatus is a configured deployment plus its recent run history,
// ready for the monitor UI.
type DeploymentStatus struct {
	App    string `json:"app"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Environment is the first-class environment this deployment targets
	// (production, staging, ...); it and App together identify the deployment.
	Environment string `json:"environment"`
	// WorkflowURL is the deploy workflow file's Actions page on github.com (where
	// the run history lives). This is the value the field previously named "url"
	// carried; it was renamed so its meaning is explicit alongside LiveURL.
	WorkflowURL string `json:"workflowUrl"`
	// LiveURL points at the running app itself: the server's custom domain when
	// one is configured, otherwise the server's IP and the app's deploy port.
	// Empty when the app's address cannot be derived.
	LiveURL string `json:"liveUrl"`
	// Port is the deployment's primary published host port, used by the UI to
	// scope custom domains to this environment (only domains whose upstream
	// matches show on its card). Zero when no port was configured.
	Port       int           `json:"port"`
	ServerID   string        `json:"serverId"`
	Status     string        `json:"status"` // live | idle | failing
	LastDeploy string        `json:"lastDeploy"`
	Runs       []RunView     `json:"runs"`
	Deploys    int           `json:"deploys"`  // total times deployed (from the tracking log)
	Timeline   []ReleaseView `json:"timeline"` // recent releases, newest first
	// Locked is true while a deploy to this target is in flight (the local deploy
	// lock is held); LockedSince is a relative time like "2m ago". The UI shows a
	// lock indicator and offers a "clear stuck lock" action.
	Locked      bool   `json:"locked"`
	LockedSince string `json:"lockedSince,omitempty"`
}

// monitorRunLimit caps recent runs; monitorEventLimit caps the deploy timeline.
const (
	monitorRunLimit   = 5
	monitorEventLimit = 10
	monitorFetchLimit = 8 // concurrent per-deployment GitHub/DB fetches
)

// Deploy artifact repo paths a deployment commits, reconstructed on teardown to
// remove them: the deploy script (repo root) and the per-branch workflow (under
// .github/workflows by its stored file name).
const (
	teardownWorkflowDir = ".github/workflows/"
)

// MonitorService reports deploy history: the configured deployments enriched
// with their recent GitHub Actions runs, read on the connected user's behalf.
// It also tears a deployment down on request: stopping its container(s) on the
// server, removing its committed deploy workflow, and forgetting its tracking.
type MonitorService struct {
	deployments DeploymentStore
	releases    ReleaseReader
	deleter     DeploymentDeleter
	tokens      TokenStore
	runs        WorkflowRunLister
	servers     ServerStore
	vault       SecretVault
	purger      AppPurger
	repoFiles   RepoFileDeleter
	repoEnvs    RepoEnvironmentDeleter
	repoKeys    RepoKeyDeleter
	ports       DeploymentPortSetter
	locker      *DeployLocker
	log         *slog.Logger
}

// NewMonitorService wires the service to its ports. The server store is read to
// derive each deployment's live app URL (its domain, or its IP) and to reach the
// server on teardown; the vault holds the per-server key used to SSH in and is
// also used to clear the stored deploy key when the deployment is removed; the
// teardown stops the container; repoFiles removes the committed workflow and
// deploy script; repoEnvs removes the GitHub deployment environment (and its
// scoped secrets); repoKeys removes the deploy key mountabo added to the repo.
// Together they undo everything the deploy created on GitHub. The deleter then
// forgets mountabo's tracking. locker is the shared deploy lock, read to surface
// lock state and cleared by the recovery action.
func NewMonitorService(deployments DeploymentStore, releases ReleaseReader, deleter DeploymentDeleter, tokens TokenStore, runs WorkflowRunLister, servers ServerStore, vault SecretVault, purger AppPurger, repoFiles RepoFileDeleter, repoEnvs RepoEnvironmentDeleter, repoKeys RepoKeyDeleter, ports DeploymentPortSetter, locker *DeployLocker, log *slog.Logger) *MonitorService {
	if log == nil {
		log = slog.Default()
	}
	return &MonitorService{deployments: deployments, releases: releases, deleter: deleter, tokens: tokens, runs: runs, servers: servers, vault: vault, purger: purger, repoFiles: repoFiles, repoEnvs: repoEnvs, repoKeys: repoKeys, ports: ports, locker: locker, log: log}
}

// ClearLock force-releases the deploy lock for a deployment (owner, repo,
// environment) — the "clear stuck lock" recovery action — and reports whether a
// lock was actually held.
func (s *MonitorService) ClearLock(owner, repo, environment string) bool {
	return s.locker.Clear(lockKey(owner, repo, environment))
}

// SetPort records the published port for the deployment identified by its full
// identity (owner, repo, environment). It is how a domain attached after deploy
// tells mountabo which port the app serves on, so the domain associates with the
// deployment. ErrDeploymentNotFound when no such deployment exists (including if
// it vanished between the store write and now).
func (s *MonitorService) SetPort(owner, repo, environment string, port int) error {
	updated, err := s.ports.SetPort(owner, repo, environment, port)
	if err != nil {
		return fmt.Errorf("set deployment port: %w", err)
	}
	if !updated {
		return ErrDeploymentNotFound
	}
	return nil
}

// Delete tears a deployment down by its identity (owner, repo, environment): it
// disables the trigger, removes what the deployment owns on its server, drops the
// GitHub environment and (when nothing else needs it) the repo deploy key, then
// forgets mountabo's tracking.
//
// The server-side work is best-effort and reports what it could not remove rather
// than failing the delete: a user must never be stuck with an undeletable
// deployment, but must also never be told "done" while containers and a plaintext
// .env are still on the box.
//
// It holds the deploy lock throughout. Deleting while a deploy mountabo started is
// mid-flight would race the very resources being removed.
// ErrDeploymentNotFound is returned when no such deployment exists.
func (s *MonitorService) Delete(ctx context.Context, owner, repo, environment string, opts DeleteOptions, out io.Writer) error {
	// Load the record first so the teardown knows which server and repo files to
	// reach. A missing record is the not-found case; a store read failure is real.
	dep, found, err := s.lookup(owner, repo, environment)
	if err != nil {
		return fmt.Errorf("look up deployment: %w", err)
	}
	if !found {
		return ErrDeploymentNotFound
	}

	// Serialize against mountabo's own deploys. The tombstone the purge leaves
	// behind is what stops a push-triggered run recreating the app afterwards.
	key := lockKey(owner, repo, environment)
	if !s.locker.TryAcquire(key) {
		return ErrDeployInProgress
	}
	defer s.locker.Release(key)

	s.tearDown(ctx, dep, opts, out)

	removed, err := s.deleter.Delete(dep.Owner, dep.Repo, dep.Environment)
	if err != nil {
		return fmt.Errorf("delete deployment: %w", err)
	}
	if !removed {
		// The record vanished between the lookup and the delete (a concurrent
		// delete); treat it as already gone.
		return ErrDeploymentNotFound
	}
	return nil
}

// lookup finds the tracked deployment by its identity (owner, repo,
// environment) — unique, unlike an app name, which two repos can share. found is
// false when no deployment matches.
func (s *MonitorService) lookup(owner, repo, environment string) (Deployment, bool, error) {
	deployments, err := s.deployments.List()
	if err != nil {
		return Deployment{}, false, fmt.Errorf("list deployments: %w", err)
	}
	for _, d := range deployments {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			return d, true, nil
		}
	}
	return Deployment{}, false, nil
}

// tearDown stops the app's container(s) on its server and removes its committed
// deploy workflow + deploy.sh from the repo. Every step is best-effort: each
// failure is logged and the rest still run, so a partial teardown never blocks
// forgetting the record. A server that is not set up, or a token that cannot be
// loaded, simply skips the step it gates.
func (s *MonitorService) tearDown(ctx context.Context, dep Deployment, opts DeleteOptions, out io.Writer) {
	// Disable the trigger FIRST. A push that lands mid-purge would otherwise
	// recreate what is being removed; the workflow file is the trigger, so it goes
	// before anything on the server does.
	s.removeWorkflowFiles(ctx, dep)

	// The deploy key is per REPO, not per deployment. Removing it while another
	// environment of the same repo still deploys would break that deployment's
	// clone — so it is only removed when this is the last one.
	last := s.isLastEnvironmentOfRepo(dep)
	s.purgeServer(ctx, dep, opts, last, out)
	s.removeEnvironment(ctx, dep)
	if last {
		s.removeDeployKey(ctx, dep)
	} else {
		s.log.Info("teardown: keeping the repo deploy key, another environment still uses it",
			"repo", dep.Owner+"/"+dep.Repo)
	}
}

// DeleteOptions are the operator's choices for a teardown.
type DeleteOptions struct {
	// RemoveVolumes deletes the project's named volumes, which destroys databases
	// and uploads. Off unless the operator explicitly asks.
	RemoveVolumes bool
}

// isLastEnvironmentOfRepo reports whether this is the only deployment left for
// its (owner, repo). Resources shared across a repo's environments — the deploy
// key on the box, the key on GitHub, the vault entry — may only be removed when
// nothing else is using them.
func (s *MonitorService) isLastEnvironmentOfRepo(dep Deployment) bool {
	deployments, err := s.deployments.List()
	if err != nil {
		// Unknown means "assume something else needs it". Leaving a key behind is
		// recoverable; breaking a sibling deployment's clone is not.
		s.log.Warn("teardown: could not check sibling environments, keeping shared resources", "err", err)
		return false
	}
	for _, d := range deployments {
		if d.Owner == dep.Owner && d.Repo == dep.Repo && d.Environment != dep.Environment {
			return false
		}
	}
	return true
}

// purgeServer removes what this deployment owns on its server, by ownership label
// and validated path. Skipped (with a log) when the server cannot be reached.
func (s *MonitorService) purgeServer(ctx context.Context, dep Deployment, opts DeleteOptions, last bool, out io.Writer) {
	server, err := s.servers.Get(dep.ServerID)
	if err != nil {
		s.log.Warn("teardown: skip server cleanup, server unavailable", "serverId", dep.ServerID, "err", err)
		return
	}
	if server.Status != StatusReady {
		s.log.Warn("teardown: skip server cleanup, server not set up", "serverId", dep.ServerID)
		return
	}
	key, err := s.vault.LoadSecret(privateKeyKey(dep.ServerID))
	if err != nil {
		s.log.Warn("teardown: skip server cleanup, key unavailable", "serverId", dep.ServerID, "err", err)
		return
	}
	t := SSHTarget{Host: server.IP, Port: server.SSHPort, User: BootstrapUser, PrivateKey: key, Fingerprint: server.Fingerprint}

	spec := PurgeSpec{
		DeploymentID:    DeploymentIDFor(dep),
		RemoveVolumes:   opts.RemoveVolumes,
		RemoveDeployKey: last,
	}
	if dep.Config != nil {
		spec.DeployDir = dep.Config.DeployDir
		spec.DeployKeyFile = dep.Config.DeployKeyFile
	}
	if err := s.purger.PurgeApp(ctx, t, spec, out); err != nil {
		// Reported rather than swallowed: a teardown that claims success while
		// leaving containers and a plaintext .env behind is worse than one that
		// says what it could not remove.
		s.log.Warn("teardown: server cleanup incomplete", "deployment", spec.DeploymentID, "err", err)
	}
}

// removeEnvironment deletes the GitHub deployment environment mountabo created
// for this deployment, which also drops the encrypted secrets scoped to it
// (SERVER_HOST/USER/SSH_KEY/DEPLOY_DIR and the operator's variables). Skipped
// (with a log) when no token is stored or the deployment has no environment
// recorded.
func (s *MonitorService) removeEnvironment(ctx context.Context, dep Deployment) {
	if dep.Environment == "" {
		return
	}
	token, err := s.tokens.Load()
	if err != nil {
		s.log.Warn("teardown: skip environment removal, github token unavailable", "app", dep.App, "err", err)
		return
	}
	if err := s.repoEnvs.DeleteEnvironment(ctx, token, dep.Owner, dep.Repo, dep.Environment); err != nil {
		s.log.Warn("teardown: delete deployment environment failed", "app", dep.App, "repo", dep.Owner+"/"+dep.Repo, "env", dep.Environment, "err", err)
	}
}

// removeDeployKey removes mountabo's read-only deploy keys from the repo (by
// title prefix) and clears the matching private key from the local vault so the
// stored secret does not outlive the GitHub side. Skipped (with a log) when no
// token is stored. Vault clearing is best-effort: a missing entry is fine.
func (s *MonitorService) removeDeployKey(ctx context.Context, dep Deployment) {
	token, err := s.tokens.Load()
	if err != nil {
		s.log.Warn("teardown: skip deploy key removal, github token unavailable", "app", dep.App, "err", err)
		return
	}
	if err := s.repoKeys.DeleteDeployKey(ctx, token, dep.Owner, dep.Repo, "mountabo deploy"); err != nil {
		s.log.Warn("teardown: delete deploy key failed", "app", dep.App, "repo", dep.Owner+"/"+dep.Repo, "err", err)
	}
	if err := s.vault.DeleteSecret(deployKeyVaultKey(dep.Owner, dep.Repo)); err != nil {
		s.log.Warn("teardown: clear stored deploy key failed", "app", dep.App, "repo", dep.Owner+"/"+dep.Repo, "err", err)
	}
}

// removeWorkflowFiles deletes the deployment's committed deploy workflow and
// deploy.sh from the repo, so a future push has no deploy workflow to run. It is
// skipped (with a log) when no GitHub token is stored; each file is deleted
// independently so one missing file does not stop the other.
func (s *MonitorService) removeWorkflowFiles(ctx context.Context, dep Deployment) {
	token, err := s.tokens.Load()
	if err != nil {
		s.log.Warn("teardown: skip workflow removal, github token unavailable", "app", dep.App, "err", err)
		return
	}
	// This deployment's own script. There is no repo-root deploy.sh to also
	// remove: the script has always been committed per deployment.
	paths := []string{workflow.ScriptPath(workflow.Config{
		Environment:  dep.Environment,
		DeploymentID: DeploymentIDFor(dep),
	})}
	if dep.WorkflowFile != "" {
		paths = append(paths, teardownWorkflowDir+dep.WorkflowFile)
	}
	for _, path := range paths {
		if err := s.repoFiles.DeleteFile(ctx, token, dep.Owner, dep.Repo, dep.Branch, path, "mountabo: remove deploy on teardown"); err != nil {
			s.log.Warn("teardown: delete repo file failed", "app", dep.App, "repo", dep.Owner+"/"+dep.Repo, "path", path, "err", err)
		}
	}
}

// History lists every configured deployment with its recent runs. It returns
// ErrNotConnected when no token is stored. A deployment whose runs can't be read
// (repo deleted, permissions, no runs yet) still appears, just with no runs.
func (s *MonitorService) History(ctx context.Context) ([]DeploymentStatus, error) {
	deployments, err := s.deployments.List()
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	if len(deployments) == 0 {
		return []DeploymentStatus{}, nil
	}

	token, err := s.tokens.Load()
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}

	// Index the servers by id so each deployment can derive its live app URL
	// (domain or IP) without a per-deployment lookup. A failure to read them is
	// not fatal: deployments still appear, just without a live URL.
	serversByID := map[string]Server{}
	if servers, serr := s.servers.List(); serr == nil {
		for _, sv := range servers {
			serversByID[sv.ID] = sv
		}
	}

	// Each deployment needs an independent GitHub Actions lookup, so fan them out
	// (bounded) rather than paying for them one after another: the page used to
	// block on the sum of every deployment's round-trip. Each goroutine writes
	// its own slot, so the result keeps the deployment order without locking.
	out := make([]DeploymentStatus, len(deployments))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(monitorFetchLimit)
	for i, d := range deployments {
		g.Go(func() error {
			// Best-effort per deployment: a repo that's deleted, lacks
			// permission, or has no runs yet still appears, just without runs.
			runs, _ := s.runs.ListWorkflowRuns(gctx, token, d.Owner, d.Repo, d.WorkflowFile, d.Branch, monitorRunLimit)
			releases, total, _ := s.releases.Releases(d.Owner, d.Repo, d.Environment, monitorEventLimit)
			status := buildStatus(d, serversByID[d.ServerID], runs, releases, total)
			// Overlay the live deploy-lock state (keyed by app + environment) so the
			// card can show a lock indicator while a deploy to this target is in flight.
			if since, locked := s.locker.Held(lockKey(d.Owner, d.Repo, d.Environment)); locked {
				status.Locked = true
				status.LockedSince = relativeTime(since)
			}
			out[i] = status
			return nil
		})
	}
	_ = g.Wait() // goroutines are best-effort and never return an error; Wait only joins them
	return out, nil
}

// buildStatus assembles a deployment's UI status from its recent runs (newest
// first); the latest run sets the headline status and last-deploy time. server
// is the deployment's target (zero value when it can't be resolved), used to
// derive the live app URL.
func buildStatus(d Deployment, server Server, runs []WorkflowRun, releases []Release, deploys int) DeploymentStatus {
	st := DeploymentStatus{
		App:         d.App,
		Repo:        d.Owner + "/" + d.Repo,
		Branch:      d.Branch,
		Environment: d.Environment,
		ServerID:    d.ServerID,
		WorkflowURL: fmt.Sprintf("https://github.com/%s/%s/actions/workflows/%s", d.Owner, d.Repo, d.WorkflowFile),
		LiveURL:     liveURL(server, d.Port),
		Port:        d.Port,
		Status:      "idle",
		LastDeploy:  "n/a",
		Runs:        make([]RunView, 0, len(runs)),
		Deploys:     deploys,
		Timeline:    make([]ReleaseView, 0, len(releases)),
	}
	// Index runs by their head SHA so each stored release can be joined to the
	// run that built it and pick up the run's live status and URL.
	runBySHA := make(map[string]WorkflowRun, len(runs))
	for _, r := range runs {
		if r.SHA != "" {
			runBySHA[r.SHA] = r
		}
	}
	for i, r := range runs {
		view := RunView{
			SHA:       shortSHA(r.SHA),
			Message:   r.Title,
			Status:    runStatus(r),
			When:      relativeTime(r.CreatedAt),
			Duration:  runDuration(r),
			RunURL:    r.HTMLURL,
			CommitURL: commitURL(d.Owner, d.Repo, r.SHA),
		}
		st.Runs = append(st.Runs, view)
		if i == 0 {
			st.Status = deployStatus(view.Status)
			st.LastDeploy = view.When
		}
	}
	for _, rel := range releases {
		st.Timeline = append(st.Timeline, releaseView(rel, runBySHA))
	}
	return st
}

// releaseView shapes a stored release for the UI, overlaying the live status and
// run URL from the Actions run that built it (matched by commit SHA). A release
// with no matching run yet (still pending, or its run aged out of the recent
// list) falls back to the durable stored status.
func releaseView(rel Release, runBySHA map[string]WorkflowRun) ReleaseView {
	status, runURL := rel.Status, rel.RunURL
	if run, ok := runBySHA[rel.CommitSHA]; ok && rel.CommitSHA != "" {
		status = runStatus(run)
		runURL = run.HTMLURL
	}
	return ReleaseView{
		When:        relativeTime(rel.At),
		Environment: rel.Environment,
		SHA:         shortSHA(rel.CommitSHA),
		Status:      status,
		Actor:       rel.Actor,
		RunURL:      runURL,
	}
}

// commitURL is the github.com page for a run's commit, or "" when the sha is
// unknown.
func commitURL(owner, repo, sha string) string {
	if sha == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, sha)
}

// liveURL points at the running app on its server: a configured custom domain
// over HTTPS when one exists, otherwise the server's IP over HTTP with the
// app's port. The port is taken from a domain's upstream when one is set;
// absent that it is unknown, so it is omitted (a plain host URL). It returns ""
// when the server (and so its address) is unknown.
func liveURL(server Server, port int) string {
	// Prefer the domain that fronts THIS deployment — the one whose nginx upstream
	// is the deployment's published port — not just the server's first domain
	// (a server can host several deployments, each with its own domain).
	if d, ok := domainForPort(server, port); ok {
		return "https://" + d.Host
	}
	if server.IP == "" {
		return ""
	}
	return "http://" + server.IP
}

// domainForPort returns the server domain whose nginx upstream is port, so a
// deployment is matched to its own domain. No match (including port 0, meaning
// the deployment has no recorded port) returns ok=false.
func domainForPort(server Server, port int) (Domain, bool) {
	if port <= 0 {
		return Domain{}, false
	}
	want := strconv.Itoa(port)
	for _, d := range server.Domains {
		if d.Upstream == want {
			return d, true
		}
	}
	return Domain{}, false
}

// runStatus collapses GitHub's status+conclusion into the UI's three states.
func runStatus(r WorkflowRun) string {
	if r.Status != "completed" {
		return "running"
	}
	if r.Conclusion == "success" {
		return "success"
	}
	return "failed"
}

func deployStatus(latestRun string) string {
	switch latestRun {
	case "success", "running":
		return "live"
	case "failed":
		return "failing"
	default:
		return "idle"
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "n/a"
	}
	return sha
}

func runDuration(r WorkflowRun) string {
	if r.Status != "completed" || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return "n/a"
	}
	d := r.UpdatedAt.Sub(r.CreatedAt)
	if d < 0 {
		return "n/a"
	}
	if m := int(d.Minutes()); m > 0 {
		return fmt.Sprintf("%dm %ds", m, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "n/a"
	}
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
