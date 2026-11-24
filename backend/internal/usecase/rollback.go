package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

var (
	// ErrArtifactNotRetained is returned when the requested release's bundle is no
	// longer on the server. Retention keeps the active release and the one before
	// it, so an older release is not a rollback target however much history shows.
	ErrArtifactNotRetained = errors.New("that release is no longer retained on the server")

	// ErrNoPreviousRelease is returned when an environment has never had a second
	// successful release, so there is nothing earlier to go back to.
	ErrNoPreviousRelease = errors.New("this environment has no earlier retained release to roll back to")

	// ErrRunInProgress is returned when an Actions run for this target is queued or
	// in flight. It is advisory — the lock on the server is the real guarantee —
	// but failing fast here gives a clear message instead of a long lock wait.
	ErrRunInProgress = errors.New("a deploy run is in flight for this deployment")

	// ErrDeploymentBlocked is returned when the deployment's state document says a
	// migration outcome is unresolved. Nothing may run until a human resolves it.
	ErrDeploymentBlocked = errors.New("this deployment is blocked on an unresolved migration outcome")
)

// ReleaseState is the deployment's state document as the server holds it. It is
// the authority on which artifact is active, which preceded it and which is in
// flight — mountabo observes it rather than tracking those itself, because a
// push-triggered deploy happens with mountabo closed.
type ReleaseState struct {
	Generation  int    `json:"generation"`
	Lifecycle   string `json:"lifecycle"`
	Active      string `json:"active"`
	Previous    string `json:"previous"`
	Candidate   string `json:"candidate"`
	Running     string `json:"running"`
	Incarnation int    `json:"incarnation"`
	UpdatedAt   string `json:"updated_at"`
}

// ReleaseStateReader reads a deployment's state document over SSH.
type ReleaseStateReader interface {
	ReadReleaseState(ctx context.Context, t SSHTarget, deploymentID string) (ReleaseState, error)
}

// DeployScriptRunner pipes a generated script to the server and runs it as the
// deploy user, streaming combined output to out.
//
// Deliberately not as root: the deploy directory, the compose project and the
// release bundles all belong to the deploy user, and that user is in the docker
// group from bootstrap. Running these as root would leave root-owned files in a
// tree the next deploy writes to as the deploy user.
type DeployScriptRunner interface {
	RunAsDeployUser(ctx context.Context, t SSHTarget, script string, out io.Writer) error
}

// RollbackInput names one rollback. ArtifactID empty means "the release before
// the active one", which is the common case and the one the UI offers first.
type RollbackInput struct {
	Owner       string
	Repo        string
	Environment string
	ArtifactID  string
	// Force skips the advisory in-flight-run check. The lock on the server still
	// applies, so this cannot produce a concurrent activation — it only bypasses
	// the fast-fail when the operator knows the run is stuck.
	Force bool
}

// RollbackService re-points a deployment at a retained artifact, and redeploys
// the artifact already active.
//
// Both run SSH-direct rather than through Actions. Recovery should not depend on
// GitHub being reachable and a runner being available — that is precisely when
// you least want another moving part — and re-pointing to an image already on the
// box takes seconds where a run takes minutes.
type RollbackService struct {
	deployments DeploymentStore
	servers     ServerStore
	vault       SecretVault
	tokens      TokenStore
	runs        WorkflowRunLister
	state       ReleaseStateReader
	scripts     DeployScriptRunner
	locker      *DeployLocker
	log         *slog.Logger
}

// NewRollbackService wires the service to its ports.
func NewRollbackService(deployments DeploymentStore, servers ServerStore, vault SecretVault, tokens TokenStore,
	runs WorkflowRunLister, state ReleaseStateReader, scripts DeployScriptRunner, locker *DeployLocker, log *slog.Logger) *RollbackService {
	return &RollbackService{
		deployments: deployments, servers: servers, vault: vault, tokens: tokens,
		runs: runs, state: state, scripts: scripts, locker: locker, log: log,
	}
}

// State returns the deployment's release state as the server holds it.
func (s *RollbackService) State(ctx context.Context, owner, repo, environment string) (ReleaseState, error) {
	dep, target, err := s.resolve(owner, repo, environment)
	if err != nil {
		return ReleaseState{}, err
	}
	return s.state.ReadReleaseState(ctx, target, DeploymentIDFor(dep))
}

// Rollback re-points the deployment at a retained artifact, streaming progress to
// out. It never rebuilds: the image it starts is the one that release ran.
func (s *RollbackService) Rollback(ctx context.Context, in RollbackInput, out io.Writer) error {
	return s.run(ctx, in, "rollback", out)
}

// Redeploy re-points the deployment at the artifact already active — a restart
// from the exact image that is supposed to be running, with no rebuild and no
// dependence on GitHub. It is the same code path as Rollback, so it cannot
// produce a different image than the one recorded as active.
func (s *RollbackService) Redeploy(ctx context.Context, owner, repo, environment string, out io.Writer) error {
	return s.run(ctx, RollbackInput{Owner: owner, Repo: repo, Environment: environment}, "redeploy", out)
}

func (s *RollbackService) run(ctx context.Context, in RollbackInput, kind string, out io.Writer) error {
	dep, target, err := s.resolve(in.Owner, in.Repo, in.Environment)
	if err != nil {
		return err
	}
	if dep.Config == nil {
		return ErrLegacyDeployment
	}

	// Serialize against mountabo's own operations. The lock on the server is what
	// serializes against a push-triggered run, which this cannot see.
	key := lockKey(in.Owner, in.Repo, in.Environment)
	if !s.locker.TryAcquire(key) {
		return ErrDeployInProgress
	}
	defer s.locker.Release(key)

	deploymentID := DeploymentIDFor(dep)
	state, err := s.state.ReadReleaseState(ctx, target, deploymentID)
	if err != nil {
		return fmt.Errorf("read release state: %w", err)
	}
	if state.Lifecycle == "blocked" {
		return ErrDeploymentBlocked
	}

	artifact, err := resolveArtifact(in, kind, state)
	if err != nil {
		return err
	}

	// Advisory only: GitHub cannot tell you a run is about to start, so this is a
	// clear message in the common case rather than a guarantee.
	if !in.Force {
		if err := s.assertNoRunInFlight(ctx, dep); err != nil {
			return err
		}
	}

	progress(out, "%s %s to %s", kind, deploymentID, artifact)
	script := rollbackScript(dep, artifact, kind)
	if err := s.scripts.RunAsDeployUser(ctx, target, script, out); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	progress(out, "%s complete", kind)
	return nil
}

// resolveArtifact picks the artifact to re-point to.
//
// A rollback with no explicit target means "the release before the active one" —
// the state document's previous pointer, which is the only earlier release
// retention guarantees is still on the box. A redeploy always targets the active
// one, so it can never start something other than what is supposed to be running.
func resolveArtifact(in RollbackInput, kind string, state ReleaseState) (string, error) {
	if kind == "redeploy" {
		if state.Active == "" {
			return "", ErrNoPreviousRelease
		}
		return state.Active, nil
	}
	if a := strings.TrimSpace(in.ArtifactID); a != "" {
		// Only a retained artifact can be started. History may list many more.
		if a != state.Active && a != state.Previous {
			return "", fmt.Errorf("%w: %s", ErrArtifactNotRetained, a)
		}
		return a, nil
	}
	if state.Previous == "" {
		return "", ErrNoPreviousRelease
	}
	return state.Previous, nil
}

// assertNoRunInFlight fails fast when GitHub reports a queued or running deploy.
func (s *RollbackService) assertNoRunInFlight(ctx context.Context, dep Deployment) error {
	token, err := s.tokens.Load()
	if err != nil {
		// Without a token this check simply cannot run. The server lock still
		// serializes, so this is not a reason to refuse the rollback.
		return nil //nolint:nilerr // best-effort pre-check; the server lock is the real guard
	}
	runs, err := s.runs.ListWorkflowRuns(ctx, token, dep.Owner, dep.Repo, dep.WorkflowFile, dep.Branch, 1)
	if err != nil || len(runs) == 0 {
		return nil //nolint:nilerr // GitHub unreachable must not block recovery; the server lock still serializes
	}
	if st := strings.ToLower(runs[0].Status); st != "" && st != "completed" {
		return fmt.Errorf("%w (status %s)", ErrRunInProgress, runs[0].Status)
	}
	return nil
}

// resolve finds the deployment and builds an SSH target for its server.
func (s *RollbackService) resolve(owner, repo, environment string) (Deployment, SSHTarget, error) {
	deployments, err := s.deployments.List()
	if err != nil {
		return Deployment{}, SSHTarget{}, fmt.Errorf("list deployments: %w", err)
	}
	var dep Deployment
	found := false
	for _, d := range deployments {
		if d.Owner == owner && d.Repo == repo && d.Environment == environment {
			dep, found = d, true
			break
		}
	}
	if !found {
		return Deployment{}, SSHTarget{}, ErrDeploymentNotFound
	}
	server, err := s.servers.Get(dep.ServerID)
	if err != nil {
		return dep, SSHTarget{}, fmt.Errorf("load server: %w", err)
	}
	if server.Status != StatusReady {
		return dep, SSHTarget{}, fmt.Errorf("server %s is not set up", server.Name)
	}
	key, err := s.vault.LoadSecret(privateKeyKey(dep.ServerID))
	if err != nil {
		return dep, SSHTarget{}, fmt.Errorf("load server key: %w", err)
	}
	return dep, SSHTarget{
		Host: server.IP, Port: server.SSHPort, User: BootstrapUser,
		PrivateKey: key, Fingerprint: server.Fingerprint,
	}, nil
}

// ParseReleaseState decodes a state document, tolerating an absent one.
//
// A deployment that has not run since release tracking shipped simply has no
// document; that is an empty state, not an error, so the UI can still show the
// deployment rather than failing to load it.
func ParseReleaseState(raw string) (ReleaseState, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ReleaseState{}, nil
	}
	var st ReleaseState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return ReleaseState{}, fmt.Errorf("parse release state: %w", err)
	}
	return st, nil
}

// rollbackScript is the small wrapper mountabo pipes to bash on the server. It
// sets the mode markers and then execs the deployment's own committed script.
//
// It execs the copy of the deploy script kept inside the TARGET release's own
// bundle, not the one currently committed. The bundle records a format the script
// that wrote it understands; handing an older bundle to a newer script is exactly
// the compatibility break the format version exists to flag, and the bundled copy
// removes the question entirely.
//
// It is deliberately tiny. Every decision — how to pin images, how to activate,
// how to verify, how to move the release pointers — lives in that committed
// script, which the operator can read in their own repository. A second
// implementation here would drift from the deploy path it is meant to undo, and
// a rollback that has drifted is worse than no rollback.
func rollbackScript(dep Deployment, artifact, kind string) string {
	// The id and artifact are interpolated as quoted shell VALUES and the path is
	// assembled from them, rather than quoting a whole path containing $HOME —
	// inside single quotes $HOME would be a literal and the script would not be
	// found.
	return fmt.Sprintf(`set -euo pipefail
export MOUNTABO_MODE=rollback
export MOUNTABO_KIND=%s
export MOUNTABO_ARTIFACT=%s
export DEPLOY_DIR=%s
deployment=%s
artifact=%s
script="$HOME/.mountabo/releases/$deployment/$artifact/runner.sh"
if [ ! -f "$script" ]; then
  echo "error: release $artifact is no longer retained on this server" >&2
  echo "       mountabo keeps the active release and the one before it" >&2
  exit 1
fi
exec bash "$script" %s
`,
		shellSingleQuoteValue(kind),
		shellSingleQuoteValue(artifact),
		shellSingleQuoteValue(dep.Config.DeployDir),
		shellSingleQuoteValue(DeploymentIDFor(dep)),
		shellSingleQuoteValue(artifact),
		shellSingleQuoteValue(dep.Branch),
	)
}

// shellSingleQuoteValue wraps a value in single quotes for the shell, escaping
// any embedded single quote. Values here are generated ids and configured paths,
// but they cross a shell boundary, so they are escaped rather than trusted.
func shellSingleQuoteValue(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ApproveInput authorises one candidate's gated hooks.
//
// An approval names an artifact AND the fingerprint of the hook set it was read
// against. Both must still match when it is used: if a newer deploy has
// superseded the candidate, or the operator edited a command after approving,
// the approval authorises nothing. They approved what they read, at the moment
// they read it.
type ApproveInput struct {
	Owner       string
	Repo        string
	Environment string
	// ArtifactID is the candidate being approved.
	ArtifactID string
	// HooksVersion fingerprints the hook set shown to the approver.
	HooksVersion string
	// HookIDs are the gated hooks this approval covers.
	HookIDs []string
	// Actor is who approved, recorded so the run says who authorised it.
	Actor string
}

// ApproveAndContinue runs the gated hooks for a candidate and, when they pass,
// continues the deploy that was halted waiting for them.
//
// The halted run exited without activating anything, so the previous release is
// still serving; this is what actually applies the migration and swaps. It is the
// only caller that ever sets the approval marker.
func (s *RollbackService) ApproveAndContinue(ctx context.Context, in ApproveInput, out io.Writer) error {
	dep, target, err := s.resolve(in.Owner, in.Repo, in.Environment)
	if err != nil {
		return err
	}
	if dep.Config == nil {
		return ErrLegacyDeployment
	}

	key := lockKey(in.Owner, in.Repo, in.Environment)
	if !s.locker.TryAcquire(key) {
		return ErrDeployInProgress
	}
	defer s.locker.Release(key)

	deploymentID := DeploymentIDFor(dep)
	state, err := s.state.ReadReleaseState(ctx, target, deploymentID)
	if err != nil {
		return fmt.Errorf("read release state: %w", err)
	}
	if state.Lifecycle == "blocked" {
		return ErrDeploymentBlocked
	}
	// The approval must still be for the candidate that is actually waiting. A
	// newer deploy replaces the candidate, and approving the old one would apply
	// a migration for a release that is never going to be activated.
	if state.Candidate == "" || state.Candidate != in.ArtifactID {
		return fmt.Errorf("%w: the candidate is now %q", ErrApprovalStale, state.Candidate)
	}
	if v := HooksVersion(dep.Config.Hooks); v != in.HooksVersion {
		return fmt.Errorf("%w: the hooks changed since it was reviewed", ErrApprovalStale)
	}

	progress(out, "running approved hooks for %s", in.ArtifactID)
	script := approvalScript(dep, in, deploymentID)
	if err := s.scripts.RunAsDeployUser(ctx, target, script, out); err != nil {
		return fmt.Errorf("approved run: %w", err)
	}
	return nil
}

// approvalScript re-enters the deployment's committed script with the approval
// marker set.
//
// The marker is the artifact, the hook fingerprint and the environment joined
// together, so it cannot be reused for a different release, a different hook set
// or a different environment. It is set ONLY here — the generated workflow never
// forwards it, which is what stops a push running a gated migration by accident.
func approvalScript(dep Deployment, in ApproveInput, deploymentID string) string {
	return fmt.Sprintf(`set -euo pipefail
export MOUNTABO_MODE=deploy
export MOUNTABO_ARTIFACT_ID=%s
export MOUNTABO_APPROVAL=%s
export MOUNTABO_APPROVED_HOOKS=%s
export MOUNTABO_APPROVED_BY=%s
export DEPLOY_DIR=%s
deployment=%s
artifact=%s
script="$HOME/.mountabo/releases/$deployment/$artifact/runner.sh"
if [ ! -f "$script" ]; then
  echo "error: the candidate release $artifact is no longer on this server" >&2
  exit 1
fi
exec bash "$script" %s
`,
		shellSingleQuoteValue(in.ArtifactID),
		shellSingleQuoteValue(strings.Join([]string{in.ArtifactID, in.HooksVersion, in.Environment}, ":")),
		shellSingleQuoteValue(strings.Join(in.HookIDs, " ")),
		shellSingleQuoteValue(in.Actor),
		shellSingleQuoteValue(dep.Config.DeployDir),
		shellSingleQuoteValue(deploymentID),
		shellSingleQuoteValue(in.ArtifactID),
		shellSingleQuoteValue(dep.Branch),
	)
}
