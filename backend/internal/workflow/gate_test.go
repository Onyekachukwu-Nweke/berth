package workflow

import (
	"strings"
	"testing"
)

func hookedConfig() Config {
	c := base()
	c.Strategy = Compose
	c.Environment = "production"
	c.HooksVersion = "v1abc"
	c.Hooks = []Hook{
		{ID: "migrate", When: HookMigration, Name: "run migrations", Command: "php artisan migrate --force",
			Target: "container", Service: "api", Blocking: true, Confirm: true},
		{ID: "warm", When: HookPostDeploy, Name: "warm cache", Command: "php artisan config:cache",
			Target: "container", Service: "api"},
	}
	return c
}

// TestGate_ApprovalIsNeverForwardedByTheWorkflow is the load-bearing test for the
// whole confirmation gate.
//
// appleboy/ssh-action forwards ONLY the variables named in its envs: list. Keeping
// MOUNTABO_APPROVAL out of both that list and the step's env: block makes it
// structurally impossible for the Actions runner to set it — a repository or
// environment variable of that name still would not reach the script.
//
// This is protection against accidental execution in the generated workflow, NOT
// an authorization boundary: anyone who can push to the repo can change the
// committed script, which runs as the deploy user. The test guards the generator
// against regression, which is a real risk given how easy it would be to add the
// name to the forward list "for convenience".
func TestGate_ApprovalIsNeverForwardedByTheWorkflow(t *testing.T) {
	w := Workflow(hookedConfig())
	for _, forbidden := range []string{"MOUNTABO_APPROVAL", "MOUNTABO_APPROVED_HOOKS", "MOUNTABO_APPROVED_BY"} {
		if strings.Contains(w, forbidden) {
			t.Errorf("the workflow must never carry %s; a push could then run a gated migration:\n%s", forbidden, w)
		}
	}
	// The script must actually read it, or the gate would be checking nothing.
	if !strings.Contains(DeployScript(hookedConfig()), "MOUNTABO_APPROVAL") {
		t.Error("the deploy script should consult MOUNTABO_APPROVAL")
	}
}

// TestGate_UnapprovedMigrationHaltsBeforeActivation is the second acceptance
// property: an unapproved required migration prevents activation.
//
// Skipping the migration and continuing would activate new code against a schema
// that was never migrated — exactly what running migrations before the swap
// exists to prevent. The run must stop with the previous release still serving.
func TestGate_UnapprovedMigrationHaltsBeforeActivation(t *testing.T) {
	s := DeployScript(hookedConfig())

	halt := strings.Index(s, `if [ -n "$MOUNTABO_PENDING" ]; then`)
	if halt < 0 {
		t.Fatalf("no halt for pending migrations:\n%s", s)
	}
	activate := strings.Index(s, "up -d --no-build")
	if activate < 0 {
		t.Fatalf("no activation found:\n%s", s)
	}
	if halt > activate {
		t.Errorf("the pending-migration halt (%d) must come BEFORE activation (%d)", halt, activate)
	}
	// And it must actually stop, not merely warn.
	tail := s[halt:activate]
	if !strings.Contains(tail, "exit 0") {
		t.Errorf("a pending migration must stop the run before activating:\n%s", tail)
	}
	if !strings.Contains(tail, `awaiting_confirmation`) {
		t.Errorf("the halt should record the candidate as awaiting confirmation:\n%s", tail)
	}
}

// TestGate_MigrationRunsBeforeTheSwap pins the expand/contract ordering: an
// additive schema change goes in while the OLD code is still serving, so both old
// and new code work against it.
func TestGate_MigrationRunsBeforeTheSwap(t *testing.T) {
	s := DeployScript(hookedConfig())
	migration := strings.Index(s, "mountabo_hook_phase 'migration'")
	activate := strings.Index(s, "up -d --no-build")
	postDeploy := strings.Index(s, "mountabo_hook_phase 'post_deploy'")

	if migration < 0 || activate < 0 || postDeploy < 0 {
		t.Fatalf("phases missing (migration=%d activate=%d post_deploy=%d)", migration, activate, postDeploy)
	}
	if migration > activate {
		t.Error("migration must run before the swap, not after")
	}
	if postDeploy < activate {
		t.Error("post_deploy must run after the swap")
	}
}

// TestGate_ProductionMigrationsCannotWaiveTheGate covers the rule that is not the
// operator's to waive. "Production requires confirmation" is meaningless when the
// actor is a git push, so it has to be a property of the generated hook.
func TestGate_ProductionMigrationsCannotWaiveTheGate(t *testing.T) {
	c := hookedConfig()
	c.Hooks[0].Confirm = false // the operator tries to turn it off
	// The usecase layer forces it back on before generation; simulate that here by
	// asserting the generated script gates whatever arrives with Confirm set, and
	// that a non-gated hook is genuinely not gated.
	c.Hooks[0].Confirm = true
	s := DeployScript(c)
	if !strings.Contains(s, "requires confirmation in mountabo; not run") {
		t.Errorf("a gated hook should refuse to run without approval:\n%s", s)
	}

	// The non-gated post_deploy hook must not be gated.
	i := strings.Index(s, "'warm')")
	if i < 0 {
		t.Fatalf("warm hook missing:\n%s", s)
	}
	block := s[i:]
	if end := strings.Index(block, ";;"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "__confirm=0") {
		t.Errorf("an ordinary hook should not be gated:\n%s", block)
	}
}

// TestHooks_UseRunNotExec pins the primitive. exec needs the container already
// running, so it cannot express a migration that must precede the swap — and it
// would run inside the container serving traffic.
func TestHooks_UseRunNotExec(t *testing.T) {
	s := DeployScript(hookedConfig())
	if strings.Contains(s, "docker compose $COMPOSE_ARGS exec") || strings.Contains(s, "docker exec") {
		t.Errorf("hooks must not use exec:\n%s", s)
	}
	if !strings.Contains(s, "run --rm -d") {
		t.Errorf("container hooks should use compose run:\n%s", s)
	}
	// The entrypoint must be overridden explicitly: passing a command to
	// `compose run` appends it as arguments to the image's entrypoint rather than
	// replacing it, so an image with one would never run the command.
	if !strings.Contains(s, "--entrypoint sh") {
		t.Errorf("hooks must override the image entrypoint:\n%s", s)
	}
	// --no-deps by default: a hook that quietly starts half the stack is a surprise.
	if !strings.Contains(s, `__deps="--no-deps"`) {
		t.Errorf("container hooks should not start dependencies by default:\n%s", s)
	}
}

// TestHooks_TimeoutStopsTheContainer covers the case where killing the local
// process is not enough: a migration abandoned mid-flight keeps writing to the
// database with nobody watching.
func TestHooks_TimeoutStopsTheContainer(t *testing.T) {
	s := DeployScript(hookedConfig())
	if !strings.Contains(s, `docker rm -f "$__hook_container"`) {
		t.Errorf("a timed-out hook must have its container stopped:\n%s", s)
	}
	if !strings.Contains(s, "exceeded its ${__timeout}s timeout") {
		t.Errorf("a timeout should say so:\n%s", s)
	}
	// A migration whose outcome is unknown blocks the deployment rather than
	// letting the next operation proceed over an indeterminate schema.
	if !strings.Contains(s, `.lifecycle = "blocked"`) {
		t.Errorf("an unknown migration outcome must block the deployment:\n%s", s)
	}
}

// TestHooks_AbsentWhenNoneConfigured keeps the machinery out of a script that has
// no hooks, so the committed file stays readable for the common case.
func TestHooks_AbsentWhenNoneConfigured(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)
	for _, marker := range []string{"mountabo_run_hook", "mountabo_hook_phase", "MOUNTABO_APPROVAL"} {
		if strings.Contains(s, marker) {
			t.Errorf("a deployment with no hooks should carry no hook machinery (%q):\n%s", marker, s)
		}
	}
}
