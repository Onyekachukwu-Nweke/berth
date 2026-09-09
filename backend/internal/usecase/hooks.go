package usecase

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/goodylili/mountabo/internal/workflow"
)

// Hook phases, in the order the deploy runs them.
//
// migration sits BEFORE the swap deliberately. That is the expand half of
// expand/contract: an additive schema change goes in while the old code is still
// serving, so both the old and new code work against it. Running it after the
// swap would mean new code briefly meets an old schema.
const (
	HookPreBuild    = "pre_build"
	HookPostBuild   = "post_build"
	HookMigration   = "migration"
	HookPreDeploy   = "pre_deploy"
	HookPostDeploy  = "post_deploy"
	HookHealthCheck = "health_check"
	HookRollback    = "rollback"
)

// hookPhaseOrder is the canonical execution order. Hooks are sorted by it so the
// generated script's order does not depend on the order rows were entered in a form.
var hookPhaseOrder = map[string]int{
	HookPreBuild: 0, HookPostBuild: 1, HookMigration: 2, HookPreDeploy: 3,
	HookPostDeploy: 4, HookHealthCheck: 5, HookRollback: 6,
}

// Hook targets.
const (
	// HookTargetContainer runs the command in a fresh container from the app's
	// image, with the app's environment.
	HookTargetContainer = "container"
	// HookTargetHost runs the command in the deploy directory on the server.
	HookTargetHost = "host"
)

var (
	// ErrInvalidHook is returned when a hook cannot be run as configured.
	ErrInvalidHook = errors.New("invalid hook")
	// ErrHookNotFound is returned when an id names no configured hook.
	ErrHookNotFound = errors.New("hook not found")
	// ErrApprovalRequired is returned when a gated hook is asked to run without a
	// matching approval.
	ErrApprovalRequired = errors.New("this hook requires confirmation")
	// ErrApprovalStale is returned when an approval names an artifact or hook set
	// that is no longer the candidate — a newer deploy has superseded it.
	ErrApprovalStale = errors.New("that approval is for a superseded release")
)

// Hook is one operator-defined command the deploy runs at a named point.
//
// It runs on the operator's own server, as their own deploy user, who is in the
// docker group — which is effectively root on that box. That is the same trust
// model as the terminal page, and worth stating rather than implying: mountabo
// never originates a hook command, it only runs what the operator wrote.
type Hook struct {
	ID   string `json:"id"`
	When string `json:"when"`
	Name string `json:"name"`
	// Command is passed to a shell inside the target. It is not parsed.
	Command string `json:"command"`
	// Dir is the working directory: repo-relative for a host hook, relative to
	// the image's WORKDIR for a container hook. Empty means no change.
	Dir string `json:"dir"`
	// Target is "container" (default) or "host".
	Target string `json:"target"`
	// Service is the compose service whose image and environment the command runs
	// in. Required for a container hook under the compose strategy; the docker
	// strategy has only one image, so it is ignored there.
	Service string `json:"service"`
	// WithDeps starts the service's depends_on before running. Off by default: a
	// hook that quietly starts half the stack is a surprise, and a migration
	// usually only needs the database, which is already up.
	WithDeps bool `json:"withDeps"`
	// Blocking fails the deploy when the command exits non-zero.
	Blocking bool `json:"blocking"`
	// Confirm makes the hook require a human. A push never runs it.
	Confirm bool `json:"confirm"`
	// TimeoutSeconds bounds the run. Zero uses hookDefaultTimeout.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// hookDefaultTimeout bounds a hook that does not set one. Migrations on a large
// table are legitimately slow, so this is generous rather than tight.
const hookDefaultTimeout = 900

// Timeout is the hook's bound in seconds.
func (h Hook) Timeout() int {
	if h.TimeoutSeconds > 0 {
		return h.TimeoutSeconds
	}
	return hookDefaultTimeout
}

// target normalises the target, defaulting to the container.
func (h Hook) target() string {
	if h.Target == HookTargetHost {
		return HookTargetHost
	}
	return HookTargetContainer
}

// RequiresConfirmation reports whether this hook is gated for the environment.
//
// A migration in production is ALWAYS gated, and the UI does not offer to turn
// that off. "Production requires confirmation" is meaningless when the actor is a
// git push, so the gate has to be a property of the hook rather than a promise in
// the docs.
func (h Hook) RequiresConfirmation(environment string) bool {
	if h.Confirm {
		return true
	}
	return h.When == HookMigration && isProductionEnvironment(environment)
}

// isProductionEnvironment recognises the environment names that get the stricter
// default. Anything unrecognised is treated as non-production, so the gate is
// never applied by surprise to a staging-like environment.
func isProductionEnvironment(environment string) bool {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "production", "prod":
		return true
	}
	return false
}

// ValidateHooks checks a hook set can actually run, and returns it in canonical
// phase order.
func ValidateHooks(hooks []Hook, strategy, environment string) ([]Hook, error) {
	seen := map[string]bool{}
	out := make([]Hook, 0, len(hooks))

	for i, h := range hooks {
		h.ID = strings.TrimSpace(h.ID)
		if h.ID == "" {
			h.ID = fmt.Sprintf("hook-%d", i+1)
		}
		if seen[h.ID] {
			return nil, fmt.Errorf("%w: duplicate hook id %q", ErrInvalidHook, h.ID)
		}
		seen[h.ID] = true

		if _, ok := hookPhaseOrder[h.When]; !ok {
			return nil, fmt.Errorf("%w: %q is not a hook phase", ErrInvalidHook, h.When)
		}
		if strings.TrimSpace(h.Command) == "" {
			return nil, fmt.Errorf("%w: %s has no command", ErrInvalidHook, h.ID)
		}
		// Validate the RAW field, not target(): that normalises anything unknown to
		// "container", so a typo like "hosts" would silently become a container
		// hook and run somewhere the operator did not intend. Empty is the default.
		switch strings.TrimSpace(h.Target) {
		case "", HookTargetContainer, HookTargetHost:
		default:
			return nil, fmt.Errorf("%w: %s has an unknown target %q (expected %q or %q)",
				ErrInvalidHook, h.ID, h.Target, HookTargetContainer, HookTargetHost)
		}

		// A container hook at pre_build has no image to run in: the candidate has
		// not been built yet. Rejecting it here beats failing on the server with a
		// confusing docker error.
		if h.target() == HookTargetContainer && h.When == HookPreBuild {
			return nil, fmt.Errorf("%w: %s runs in a container at pre_build, before any image exists — use target \"host\", or a later phase",
				ErrInvalidHook, h.ID)
		}
		// Under compose there are many images, so which service to run in cannot
		// be guessed. The docker strategy has exactly one, so the field is ignored.
		if h.target() == HookTargetContainer && strategy == string(strategyCompose) && strings.TrimSpace(h.Service) == "" {
			return nil, fmt.Errorf("%w: %s must name the compose service to run in", ErrInvalidHook, h.ID)
		}

		// Force the gate on where it is not the operator's to waive.
		if h.RequiresConfirmation(environment) {
			h.Confirm = true
		}
		out = append(out, h)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return hookPhaseOrder[out[i].When] < hookPhaseOrder[out[j].When]
	})
	return out, nil
}

// strategyCompose mirrors workflow.Compose without importing it: usecase owns its
// own vocabulary, and the strategy arrives here as a plain string on DeployConfig.
type strategyName string

const strategyCompose strategyName = "compose"

// HooksVersion is a stable fingerprint of a hook set.
//
// An approval authorises one artifact running one specific set of commands. If
// the operator edits the command after approving, the fingerprint changes and the
// approval no longer applies — which is the point: they approved what they read,
// not whatever the hook says later.
func HooksVersion(hooks []Hook) string {
	parts := make([]string, 0, len(hooks))
	for _, h := range hooks {
		parts = append(parts, strings.Join([]string{
			h.ID, h.When, h.Target, h.Service, h.Dir, h.Command,
			fmt.Sprintf("%t/%t/%t/%d", h.WithDeps, h.Blocking, h.Confirm, h.Timeout()),
		}, "\x1f"))
	}
	sort.Strings(parts)
	return shortHash(strings.Join(parts, "\x1e"))
}

// GatedHooks returns the hooks in a set that a push must not run.
func GatedHooks(hooks []Hook, environment string) []Hook {
	var out []Hook
	for _, h := range hooks {
		if h.RequiresConfirmation(environment) {
			out = append(out, h)
		}
	}
	return out
}

// shortHash is a compact, stable fingerprint. It is an identity check for an
// approval, not a security boundary, so a short digest is enough.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// WorkflowHooks maps validated hooks onto the generator's shape.
//
// The mapping exists because dependencies point inward: internal/workflow must
// not import usecase, so it restates the fields it renders and the composition
// happens here.
func WorkflowHooks(hooks []Hook) []workflow.Hook {
	out := make([]workflow.Hook, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, workflow.Hook{
			ID: h.ID, When: h.When, Name: h.Name, Command: h.Command, Dir: h.Dir,
			Target: h.target(), Service: h.Service, WithDeps: h.WithDeps,
			Blocking: h.Blocking, Confirm: h.Confirm, TimeoutSeconds: h.Timeout(),
		})
	}
	return out
}
