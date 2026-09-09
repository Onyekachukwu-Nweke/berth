package usecase

import (
	"errors"
	"strings"
	"testing"
)

func hook(id, when string, mods ...func(*Hook)) Hook {
	h := Hook{ID: id, When: when, Name: id, Command: "echo hi", Target: HookTargetContainer, Service: "api"}
	for _, m := range mods {
		m(&h)
	}
	return h
}

// TestValidateHooks_RejectsAContainerHookAtPreBuild covers a hook that cannot
// possibly run: at pre_build the candidate image does not exist yet. Rejecting it
// here beats failing on the server with a confusing docker error.
func TestValidateHooks_RejectsAContainerHookAtPreBuild(t *testing.T) {
	_, err := ValidateHooks([]Hook{hook("h1", HookPreBuild)}, "compose", "staging")
	if !errors.Is(err, ErrInvalidHook) {
		t.Fatalf("want ErrInvalidHook, got %v", err)
	}
	if !strings.Contains(err.Error(), "before any image exists") {
		t.Errorf("the error should explain why: %v", err)
	}
	// The same phase is fine on the host, which is the actual workaround.
	if _, err := ValidateHooks([]Hook{hook("h1", HookPreBuild, func(h *Hook) { h.Target = HookTargetHost })}, "compose", "staging"); err != nil {
		t.Errorf("a host hook at pre_build should be allowed: %v", err)
	}
}

func TestValidateHooks_RequiresAServiceUnderCompose(t *testing.T) {
	// Compose has many images, so which one to run in cannot be guessed.
	_, err := ValidateHooks([]Hook{hook("h1", HookMigration, func(h *Hook) { h.Service = "" })}, "compose", "staging")
	if !errors.Is(err, ErrInvalidHook) {
		t.Fatalf("want ErrInvalidHook, got %v", err)
	}
	// The docker strategy has exactly one image, so the field is not needed.
	if _, err := ValidateHooks([]Hook{hook("h1", HookMigration, func(h *Hook) { h.Service = "" })}, "docker", "staging"); err != nil {
		t.Errorf("docker strategy should not require a service: %v", err)
	}
}

func TestValidateHooks_RejectsTheUnrunnable(t *testing.T) {
	for name, hooks := range map[string][]Hook{
		"blank command":  {hook("h1", HookMigration, func(h *Hook) { h.Command = "  " })},
		"unknown phase":  {hook("h1", "whenever")},
		"unknown target": {hook("h1", HookMigration, func(h *Hook) { h.Target = "vm" })},
		"duplicate ids":  {hook("h1", HookMigration), hook("h1", HookPostDeploy)},
	} {
		if _, err := ValidateHooks(hooks, "compose", "staging"); !errors.Is(err, ErrInvalidHook) {
			t.Errorf("%s: want ErrInvalidHook, got %v", name, err)
		}
	}
}

// TestValidateHooks_OrdersByPhase keeps execution order independent of the order
// rows happened to be entered in a form.
func TestValidateHooks_OrdersByPhase(t *testing.T) {
	got, err := ValidateHooks([]Hook{
		hook("d", HookPostDeploy), hook("a", HookPreBuild, func(h *Hook) { h.Target = HookTargetHost }),
		hook("c", HookPreDeploy), hook("b", HookMigration),
	}, "compose", "staging")
	if err != nil {
		t.Fatalf("ValidateHooks: %v", err)
	}
	var order []string
	for _, h := range got {
		order = append(order, h.When)
	}
	want := []string{HookPreBuild, HookMigration, HookPreDeploy, HookPostDeploy}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("phase order = %v, want %v", order, want)
		}
	}
}

// TestHook_ProductionMigrationsAreAlwaysGated is the rule that is not the
// operator's to waive. "Production requires confirmation" is meaningless when the
// actor is a git push, so it has to be forced onto the hook itself.
func TestHook_ProductionMigrationsAreAlwaysGated(t *testing.T) {
	got, err := ValidateHooks([]Hook{hook("m", HookMigration, func(h *Hook) { h.Confirm = false })}, "compose", "production")
	if err != nil {
		t.Fatalf("ValidateHooks: %v", err)
	}
	if !got[0].Confirm {
		t.Error("a production migration must be gated even when the operator turns it off")
	}

	// Elsewhere it stays the operator's choice.
	got, err = ValidateHooks([]Hook{hook("m", HookMigration, func(h *Hook) { h.Confirm = false })}, "compose", "staging")
	if err != nil {
		t.Fatalf("ValidateHooks: %v", err)
	}
	if got[0].Confirm {
		t.Error("a staging migration should not be force-gated")
	}

	// And a non-migration hook in production is not gated by default.
	got, _ = ValidateHooks([]Hook{hook("w", HookPostDeploy, func(h *Hook) { h.Confirm = false })}, "compose", "production")
	if got[0].Confirm {
		t.Error("an ordinary production hook should not be force-gated")
	}
}

// TestHooksVersion_ChangesWhenACommandChanges is what makes an approval mean
// something. The operator approved the command they read; editing it afterwards
// must invalidate that approval rather than silently authorising the new one.
func TestHooksVersion_ChangesWhenACommandChanges(t *testing.T) {
	base := []Hook{hook("m", HookMigration, func(h *Hook) { h.Command = "migrate" })}
	v1 := HooksVersion(base)

	edited := []Hook{hook("m", HookMigration, func(h *Hook) { h.Command = "migrate --and-drop-everything" })}
	if HooksVersion(edited) == v1 {
		t.Error("editing a command must change the fingerprint, or an old approval would still authorise it")
	}

	// Reordering the same set must NOT change it: order is not part of what was
	// approved, and churn would invalidate approvals for no reason.
	two := []Hook{hook("a", HookMigration), hook("b", HookPostDeploy)}
	reordered := []Hook{two[1], two[0]}
	if HooksVersion(two) != HooksVersion(reordered) {
		t.Error("reordering the same hooks should not change the fingerprint")
	}
	// Other gate-relevant fields count too.
	blocking := []Hook{hook("m", HookMigration, func(h *Hook) { h.Blocking = true })}
	if HooksVersion(blocking) == HooksVersion([]Hook{hook("m", HookMigration)}) {
		t.Error("changing whether a hook blocks the deploy should change the fingerprint")
	}
}

func TestGatedHooks_ListsWhatAPushWillNotRun(t *testing.T) {
	hooks, err := ValidateHooks([]Hook{
		hook("m", HookMigration),
		hook("w", HookPostDeploy),
	}, "compose", "production")
	if err != nil {
		t.Fatalf("ValidateHooks: %v", err)
	}
	gated := GatedHooks(hooks, "production")
	if len(gated) != 1 || gated[0].ID != "m" {
		t.Errorf("gated = %+v, want just the production migration", gated)
	}
}
