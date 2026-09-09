package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// Hook mirrors usecase.Hook for generation. The workflow package stays free of
// usecase imports (dependencies point inward), so the fields it needs to render
// are restated here and mapped by the caller.
type Hook struct {
	ID             string
	When           string
	Name           string
	Command        string
	Dir            string
	Target         string
	Service        string
	WithDeps       bool
	Blocking       bool
	Confirm        bool
	TimeoutSeconds int
}

// Hook phase names, matching usecase.
const (
	HookPreBuild    = "pre_build"
	HookPostBuild   = "post_build"
	HookMigration   = "migration"
	HookPreDeploy   = "pre_deploy"
	HookPostDeploy  = "post_deploy"
	HookHealthCheck = "health_check"
	HookRollback    = "rollback"
)

// hookRunner is the shared shell that executes one hook.
//
// Container hooks use "compose run --rm", never "exec", and the reason is
// ordering: exec needs the target container already running, so it cannot express
// a migration that must go in BEFORE the swap — which is the only safe ordering
// for an additive schema change. run --rm also starts a fresh container from the
// service's own image rather than reaching into the one serving traffic, so a
// migration that wedges or exhausts memory cannot take production down with it,
// and it publishes no ports so it cannot collide with the running container.
//
// It is not isolated from production, and pretending otherwise would be worse
// than saying so: the hook shares the project's configured networks and volumes,
// which is exactly why a migration can reach the database.
//
// --entrypoint is set explicitly. Passing a command to "compose run" does NOT
// override an image's entrypoint — it is appended as arguments to it — so an
// image with an entrypoint would otherwise receive the command as arguments
// rather than running it.
//
// The timeout stops the CONTAINER, not just the ssh session. Killing the local
// process would leave a migration running against the database with nobody
// watching, which is the worst of both outcomes.
const hookRunner = `
# --- mountabo: release hooks --------------------------------------------------
mountabo_run_hook_command() {
  __hook_container="mountabo-hook-$DEPLOYMENT_ID-$__id"
  docker rm -f "$__hook_container" >/dev/null 2>&1 || true

  if [ "$__target" = host ]; then
    ( if [ -n "$__dir" ]; then cd "$__dir"; fi; timeout "$__timeout" sh -c "$__cmd" )
    return $?
  fi

  case "$MOUNTABO_STRATEGY" in
    compose)
      __deps="--no-deps"
      [ "$__withdeps" = 1 ] && __deps=""
      # Detached, so the timeout can stop the container itself rather than only
      # abandoning a local process.
      # shellcheck disable=SC2086
      docker compose $COMPOSE_ARGS run --rm -d --name "$__hook_container" \
        $__deps --entrypoint sh ${__dir:+-w "$__dir"} "$__svc" -lc "$__cmd" >/dev/null
      ;;
    docker)
      docker run -d --name "$__hook_container" --rm --env-file .env \
        --entrypoint sh ${__dir:+-w "$__dir"} "$IMAGE" -lc "$__cmd" >/dev/null
      ;;
  esac

  docker logs -f "$__hook_container" 2>&1 &
  __logs=$!
  if timeout "$__timeout" docker wait "$__hook_container" >/tmp/mountabo-hook-rc.$$ 2>/dev/null; then
    __rc="$(cat /tmp/mountabo-hook-rc.$$ 2>/dev/null || echo 1)"
  else
    echo "error: hook '$__name' exceeded its ${__timeout}s timeout; stopping it" >&2
    docker rm -f "$__hook_container" >/dev/null 2>&1 || true
    __rc=124
  fi
  rm -f /tmp/mountabo-hook-rc.$$
  wait "$__logs" 2>/dev/null || true
  docker rm -f "$__hook_container" >/dev/null 2>&1 || true
  return "${__rc:-1}"
}

mountabo_run_hook() {
  echo "-- $__when: $__name"

  # A gated hook is never run by a push. A push has no human at the keyboard to
  # confirm it, so the run records it as pending and moves on WITHOUT activating
  # anything downstream (see the migration phase). MOUNTABO_APPROVAL is
  # deliberately absent from the workflow's env: and envs: lists, so the Actions
  # runner cannot set it even if a repository variable of that name exists.
  #
  # That is protection against accidental execution in the generated workflow, not
  # an authorization boundary: anyone who can push to this repo can change this
  # script, which runs as the deploy user.
  if [ "$__confirm" = 1 ]; then
    if [ "${MOUNTABO_APPROVAL:-}" != "$ARTIFACT_ID:$MOUNTABO_HOOKS_VERSION:$ENVIRONMENT" ]; then
      echo "   requires confirmation in mountabo; not run"
      MOUNTABO_PENDING="${MOUNTABO_PENDING:+$MOUNTABO_PENDING }$__id"
      return 0
    fi
    case " ${MOUNTABO_APPROVED_HOOKS:-} " in
      *" $__id "*) : ;;
      *)
        echo "   not covered by the current approval; not run"
        MOUNTABO_PENDING="${MOUNTABO_PENDING:+$MOUNTABO_PENDING }$__id"
        return 0
        ;;
    esac
    echo "   approved by ${MOUNTABO_APPROVED_BY:-an operator}"
  fi

  if mountabo_run_hook_command; then
    echo "   ok"
    return 0
  fi
  __rc=$?
  if [ "$__rc" = 124 ] && [ "$__when" = migration ]; then
    # A migration whose container was stopped mid-flight may have applied some,
    # all or none of its changes. Nobody knows which, and guessing is how a schema
    # and its code silently diverge — so the deployment is blocked until a human
    # says what happened.
    mountabo_state_write '.lifecycle = "blocked" | .pending = {hook: $h, artifact: $a, state: "unknown"}' \
      --arg h "$__id" --arg a "$ARTIFACT_ID"
    echo "error: the outcome of '$__name' is unknown; this deployment is blocked until it is resolved in mountabo" >&2
    exit 1
  fi
  if [ "$__blocking" = 1 ]; then
    echo "error: '$__name' failed (exit $__rc) and is marked blocking; the deploy stops here" >&2
    return "$__rc"
  fi
  echo "   warning: '$__name' failed (exit $__rc); continuing because it is non-blocking"
  return 0
}
`

// HookDispatcher renders the case statement that resolves a hook id to its
// configuration, plus the phase driver. Returns "" when no hooks are configured,
// so a deployment without them carries no hook machinery at all.
func HookDispatcher(hooks []Hook) string {
	if len(hooks) == 0 {
		return ""
	}
	ordered := append([]Hook(nil), hooks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var b strings.Builder
	b.WriteString(hookRunner)
	b.WriteString("\nmountabo_hook() {\n  __id=\"$1\"\n  case \"$__id\" in\n")
	for _, h := range ordered {
		target := h.Target
		if target != "host" {
			target = "container"
		}
		name := h.Name
		if strings.TrimSpace(name) == "" {
			name = h.ID
		}
		fmt.Fprintf(&b, "    %s)\n", shellSingleQuote(h.ID))
		fmt.Fprintf(&b, "      __name=%s; __when=%s; __target=%s\n",
			shellSingleQuote(name), shellSingleQuote(h.When), shellSingleQuote(target))
		fmt.Fprintf(&b, "      __svc=%s; __dir=%s; __timeout=%d\n",
			shellSingleQuote(h.Service), shellSingleQuote(h.Dir), hookTimeout(h))
		fmt.Fprintf(&b, "      __confirm=%s; __blocking=%s; __withdeps=%s\n",
			boolFlag(h.Confirm), boolFlag(h.Blocking), boolFlag(h.WithDeps))
		fmt.Fprintf(&b, "      __cmd=%s\n", shellSingleQuote(h.Command))
		b.WriteString("      ;;\n")
	}
	b.WriteString("    *) echo \"unknown hook $__id\" >&2; return 1 ;;\n  esac\n  mountabo_run_hook\n}\n")

	// The phase driver. Hook ids are grouped per phase at generation time, so the
	// script does no filtering and the order is fixed here rather than discovered.
	b.WriteString("\nmountabo_hook_phase() {\n  case \"$1\" in\n")
	byPhase := map[string][]string{}
	for _, h := range hooks {
		byPhase[h.When] = append(byPhase[h.When], h.ID)
	}
	phases := make([]string, 0, len(byPhase))
	for p := range byPhase {
		phases = append(phases, p)
	}
	sort.Strings(phases)
	for _, p := range phases {
		quoted := make([]string, 0, len(byPhase[p]))
		for _, id := range byPhase[p] {
			quoted = append(quoted, shellSingleQuote(id))
		}
		fmt.Fprintf(&b, "    %s) for __h in %s; do mountabo_hook \"$__h\" || return $?; done ;;\n",
			shellSingleQuote(p), strings.Join(quoted, " "))
	}
	b.WriteString("    *) : ;;\n  esac\n}\n")
	return b.String()
}

// HookPhaseCall emits the call for one phase, or "" when nothing is configured
// for it — so the generated script stays free of no-op calls.
func HookPhaseCall(hooks []Hook, phase string) string {
	for _, h := range hooks {
		if h.When == phase {
			return fmt.Sprintf("mountabo_hook_phase %s\n", shellSingleQuote(phase))
		}
	}
	return ""
}

func hookTimeout(h Hook) int {
	if h.TimeoutSeconds > 0 {
		return h.TimeoutSeconds
	}
	return 900
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// shellSingleQuote wraps a value in single quotes, escaping embedded quotes. Hook
// commands are operator-authored and cross into a generated script, so they are
// quoted rather than trusted to be well behaved.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookContext sets the script-level values the hook machinery reads. Emitted even
// with no hooks configured, so the gate's variables are always defined and the
// script cannot trip over an unset one under "set -u".
func hookContext(c Config) string {
	return fmt.Sprintf(`
MOUNTABO_STRATEGY=%s
ENVIRONMENT=%s
MOUNTABO_HOOKS_VERSION=%s
MOUNTABO_PENDING=""
`, shellSingleQuote(string(c.strategy())), shellSingleQuote(c.environment()), shellSingleQuote(c.HooksVersion))
}

// migrationPhase runs the migration hooks and then REFUSES TO CONTINUE if any of
// them were gated and left unapproved.
//
// This is the difference between a gate and a warning. Skipping a required
// migration and carrying on would activate new code against a schema that was
// never migrated — precisely the failure the "migration before the swap" ordering
// exists to prevent. So the run stops before activation with the previous release
// still serving, records the candidate as awaiting confirmation, and exits 0: a
// deliberate gate is not a build failure, and the real signal is the banner
// mountabo shows from the state document.
func migrationPhase(c Config) string {
	call := HookPhaseCall(c.Hooks, HookMigration)
	if call == "" {
		return ""
	}
	return call + `
if [ -n "$MOUNTABO_PENDING" ]; then
  echo "::warning::deploy halted: migration(s) need confirmation in mountabo ($MOUNTABO_PENDING)"
  echo ""
  echo "The new release was built and recorded, but NOT activated."
  echo "The previous release is still serving. Confirm the migration in mountabo to continue."
  mountabo_state_write '.lifecycle = "awaiting_confirmation" | .candidate = $a | .pending = {hooks: $h, artifact: $a, version: $v, state: "awaiting"}' \
    --arg a "$ARTIFACT_ID" --arg h "$MOUNTABO_PENDING" --arg v "$MOUNTABO_HOOKS_VERSION"
  exit 0
fi
`
}

// rollbackHookPhase runs the rollback hooks, and only on a rollback.
//
// They are compensating actions the operator chooses to take when going back —
// clearing a cache, flipping a feature flag. They are explicitly NOT
// down-migrations: rolling back code does not roll back the database, and
// presenting them as if it did would be the most dangerous thing in this feature.
func rollbackHookPhase(c Config) string {
	call := HookPhaseCall(c.Hooks, HookRollback)
	if call == "" {
		return ""
	}
	return "if [ \"$MOUNTABO_MODE\" = rollback ]; then\n" + call + "fi\n"
}
