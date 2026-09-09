package workflow

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lifecycleEnv runs a fragment of the release lifecycle against a scratch state
// directory with a stubbed docker, and returns the resulting state document.
//
// These blocks are executed rather than string-matched because every property
// that matters is a runtime one: whether a state write is atomic, whether the
// pointers move correctly on a redeploy, whether pruning spares the recovery
// pair. Reading the shell cannot answer any of them.
func lifecycleEnv(t *testing.T, body, dockerStub string) (state map[string]any, out string, code int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if dockerStub == "" {
		dockerStub = "exit 0\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/usr/bin/env bash\n"+dockerStub), 0o755); err != nil {
		t.Fatalf("stub: %v", err)
	}

	script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		"DEPLOYMENT_ID=mtb-aaaaaaaaaaaa\n" +
		"MOUNTABO_STATE=" + dir + "/state-root\n" +
		"STATE_FILE=\"$MOUNTABO_STATE/state/$DEPLOYMENT_ID.json\"\n" +
		"REL_ROOT=\"$MOUNTABO_STATE/releases/$DEPLOYMENT_ID\"\n" +
		"EXPECT_INCARNATION=1\n" +
		"mkdir -p \"$MOUNTABO_STATE/state\" \"$REL_ROOT\"\n" +
		stateHelpers + "\n" + body
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := exec.Command(bash, path)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v\n%s", err, b)
		}
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "state-root", "state", "mtb-aaaaaaaaaaaa.json"))
	if rerr == nil {
		if jerr := json.Unmarshal(raw, &state); jerr != nil {
			t.Fatalf("state document is not valid JSON (%v):\n%s", jerr, raw)
		}
	}
	return state, string(b), code
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func TestReleaseState_PromotionMovesPointers(t *testing.T) {
	// Two successive releases: the second must become active and push the first
	// into previous, so exactly one release back remains recoverable.
	promote := func(id string) string {
		return "ARTIFACT_ID=" + id + "\n" + `
__prev="$(mountabo_state_get '.active')"
if [ -n "$__prev" ] && [ "$__prev" != "$ARTIFACT_ID" ]; then
  mountabo_state_write '.previous = $p | .active = $a | .candidate = null | .running = $a | .lifecycle = "active"' \
    --arg p "$__prev" --arg a "$ARTIFACT_ID"
else
  mountabo_state_write '.active = $a | .candidate = null | .running = $a | .lifecycle = "active"' \
    --arg a "$ARTIFACT_ID"
fi
`
	}
	state, out, code := lifecycleEnv(t, promote("art-1")+promote("art-2"), "")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := str(state["active"]); got != "art-2" {
		t.Errorf("active = %q, want art-2", got)
	}
	if got := str(state["previous"]); got != "art-1" {
		t.Errorf("previous = %q, want art-1", got)
	}
	if state["candidate"] != nil {
		t.Errorf("candidate should be cleared after promotion, got %v", state["candidate"])
	}
	if got := str(state["lifecycle"]); got != "active" {
		t.Errorf("lifecycle = %q, want active", got)
	}
	// generation must advance monotonically so a reader can detect a change.
	if g, _ := state["generation"].(float64); g < 2 {
		t.Errorf("generation = %v, want at least 2", state["generation"])
	}
}

// TestReleaseState_RedeployPreservesPrevious is the retention rule that is easy
// to get wrong: redeploying the artifact that is already active must NOT move
// previous onto it. Collapsing them would leave active and previous identical and
// make the release before it prunable — destroying the rollback target.
func TestReleaseState_RedeployPreservesPrevious(t *testing.T) {
	promote := func(id string) string {
		return "ARTIFACT_ID=" + id + "\n" + `
__prev="$(mountabo_state_get '.active')"
if [ -n "$__prev" ] && [ "$__prev" != "$ARTIFACT_ID" ]; then
  mountabo_state_write '.previous = $p | .active = $a | .candidate = null | .running = $a | .lifecycle = "active"' \
    --arg p "$__prev" --arg a "$ARTIFACT_ID"
else
  mountabo_state_write '.active = $a | .candidate = null | .running = $a | .lifecycle = "active"' \
    --arg a "$ARTIFACT_ID"
fi
`
	}
	// art-1, art-2, then art-2 again (a redeploy of the current release).
	state, out, code := lifecycleEnv(t, promote("art-1")+promote("art-2")+promote("art-2"), "")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := str(state["previous"]); got != "art-1" {
		t.Errorf("redeploy moved previous to %q; it must stay art-1", got)
	}
	if str(state["active"]) == str(state["previous"]) {
		t.Errorf("active and previous converged on %q", str(state["active"]))
	}
}

// TestReleaseState_SurvivesACorruptWrite checks the atomic-rename property: a
// write that fails must leave the previous document intact rather than a
// truncated one, because a torn state document is unrecoverable state.
func TestReleaseState_SurvivesACorruptWrite(t *testing.T) {
	body := `
mountabo_state_write '.active = $a | .lifecycle = "active"' --arg a "art-1"
# A filter jq cannot compile: the write must fail without damaging the document.
mountabo_state_write 'this is not valid jq('
echo DONE
`
	state, out, code := lifecycleEnv(t, body, "")
	if code != 0 {
		t.Fatalf("a failed state write aborted the script (exit %d):\n%s", code, out)
	}
	if got := str(state["active"]); got != "art-1" {
		t.Errorf("previous document was damaged by a failed write: active=%q\n%s", got, out)
	}
	if !strings.Contains(out, "could not update deployment state") {
		t.Errorf("a failed state write should say so:\n%s", out)
	}
}

// TestPrune_KeepsTheRecoveryPointsAndTakesBundlesWithTags is the retention
// property. Keeping "the newest N directories" would be wrong — a failed deploy
// or a rollback breaks that ordering — so pruning is driven by the pointers.
func TestPrune_KeepsTheRecoveryPointsAndTakesBundlesWithTags(t *testing.T) {
	// Record which tags the stub was asked to remove.
	stub := `
if [ "$1" = rmi ]; then echo "$2" >> "$MOUNTABO_RMI_LOG"; fi
exit 0
`
	body := `
export MOUNTABO_RMI_LOG="$MOUNTABO_STATE/rmi.log"
: > "$MOUNTABO_RMI_LOG"
for a in art-old art-1 art-2 art-3; do
  mkdir -p "$REL_ROOT/$a"
  jq -n --arg r "mountabo/x:$a" '{images: [{ref: $r, id: "sha256:x"}]}' > "$REL_ROOT/$a/manifest.json"
done
mountabo_state_write '.active = "art-3" | .previous = "art-2" | .candidate = "art-1"'
` + pruneBlock + `
echo "REMAINING: $(ls "$REL_ROOT" | tr '\n' ' ')"
echo "RMI: $(cat "$MOUNTABO_RMI_LOG" | tr '\n' ' ')"
`
	_, out, code := lifecycleEnv(t, body, stub)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// Take only the line each marker introduces: the two markers are adjacent in
	// the output, so splitting on one otherwise swallows the other.
	line := func(marker string) string {
		_, rest, ok := strings.Cut(out, marker)
		if !ok {
			t.Fatalf("output missing %q:\n%s", marker, out)
		}
		return strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	}
	remaining, rmi := line("REMAINING:"), line("RMI:")

	for _, keep := range []string{"art-1", "art-2", "art-3"} {
		if !strings.Contains(remaining, keep) {
			t.Errorf("%s is a recovery point but was pruned (remaining: %q)", keep, remaining)
		}
	}
	if strings.Contains(remaining, "art-old") {
		t.Errorf("art-old is not a recovery point and should have been retired (remaining: %q)", remaining)
	}
	// The bundle and its tags go together: an image whose snapshot has been
	// deleted is not recoverable, so keeping one without the other is worse than
	// keeping neither.
	if !strings.Contains(rmi, "mountabo/x:art-old") {
		t.Errorf("the retired bundle's tag was not removed with it:\n%s", out)
	}
	for _, kept := range []string{"art-1", "art-2", "art-3"} {
		if strings.Contains(rmi, "mountabo/x:"+kept) {
			t.Errorf("a retained release's tag was removed: %s\n%s", kept, out)
		}
	}
}

// TestPrune_NeverPrunesHostWide guards acceptance criterion 3 at the retention
// step. An image can carry other tags or back another deployment's containers, so
// a host-wide prune could remove a bystander's image even without -a.
func TestPrune_NeverPrunesHostWide(t *testing.T) {
	if strings.Contains(pruneBlock, "image prune") || strings.Contains(pruneBlock, "system prune") {
		t.Errorf("retention must not prune host-wide:\n%s", pruneBlock)
	}
	// And it must never force: docker refusing to remove an in-use image is the
	// correct outcome, not something to override.
	if strings.Contains(pruneBlock, "rmi -f") {
		t.Errorf("retention must not force image removal:\n%s", pruneBlock)
	}
}

// verifyEnv runs the verify-and-promote block against a stubbed docker that
// reports whichever artifact label the test wants the running containers to
// carry.
func verifyEnv(t *testing.T, servingArtifact, candidate string) (map[string]any, string, int) {
	t.Helper()
	stub := `
case "$1" in
  ps) echo cid-1 ;;
  inspect) printf '%s\n' "${MOUNTABO_TEST_SERVING}" ;;
esac
exit 0
`
	body := "export MOUNTABO_TEST_SERVING=" + shellQuoteForTest(servingArtifact) + "\n" +
		"ARTIFACT_ID=" + candidate + "\n" +
		`mountabo_state_write '.active = "art-1" | .candidate = $c' --arg c "` + candidate + `"` + "\n" +
		verifyAndPromoteBlock(base()) + "\necho PROMOTED\n"
	return lifecycleEnv(t, body, stub)
}

// TestReadiness_RejectsAnOlderContainerStillServing is the false-pass case. If
// the swap silently failed, the PREVIOUS container is still answering — so a
// plain port probe passes while the deploy reports a release that is not running.
// Readiness therefore compares the running containers' artifact label first.
func TestReadiness_RejectsAnOlderContainerStillServing(t *testing.T) {
	state, out, code := verifyEnv(t, "art-1", "art-2")
	if code == 0 || strings.Contains(out, "PROMOTED") {
		t.Errorf("promoted while the previous release was still serving:\n%s", out)
	}
	if !strings.Contains(out, "expected artifact art-2 to be serving") {
		t.Errorf("failure should name both artifacts:\n%s", out)
	}
	// The pointers must not move, and the mismatch has to be visible afterwards.
	if got := str(state["active"]); got != "art-1" {
		t.Errorf("active moved to %q despite a failed swap", got)
	}
	if got := str(state["lifecycle"]); got != "degraded" {
		t.Errorf("lifecycle = %q, want degraded", got)
	}
	if got := str(state["running"]); got != "unknown" {
		t.Errorf("running = %q, want unknown", got)
	}
}

// TestReadiness_RejectsNothingRunning covers a swap that left no container at
// all: an empty result must fail rather than read as "nothing to disagree with".
func TestReadiness_RejectsNothingRunning(t *testing.T) {
	state, out, code := verifyEnv(t, "", "art-2")
	if code == 0 || strings.Contains(out, "PROMOTED") {
		t.Errorf("promoted with no running container:\n%s", out)
	}
	if got := str(state["lifecycle"]); got != "degraded" {
		t.Errorf("lifecycle = %q, want degraded", got)
	}
}

func TestReadiness_PromotesWhenTheIntendedArtifactIsServing(t *testing.T) {
	state, out, code := verifyEnv(t, "art-2", "art-2")
	if code != 0 || !strings.Contains(out, "PROMOTED") {
		t.Fatalf("a correct swap was not promoted (exit %d):\n%s", code, out)
	}
	if got := str(state["active"]); got != "art-2" {
		t.Errorf("active = %q, want art-2", got)
	}
	if got := str(state["previous"]); got != "art-1" {
		t.Errorf("previous = %q, want art-1", got)
	}
	if state["candidate"] != nil {
		t.Errorf("candidate should be cleared, got %v", state["candidate"])
	}
}
