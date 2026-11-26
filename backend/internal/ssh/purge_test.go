package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

func purgeSpec() usecase.PurgeSpec {
	return usecase.PurgeSpec{
		DeploymentID:  "mtb-aaaaaaaaaaaa",
		DeployDir:     "/opt/shop",
		DeployKeyFile: "mountabo_deploy_acme_shop",
	}
}

// TestPurgeScript_SelectsByOwnershipNotName is the teardown half of "deleting one
// deployment cannot affect another".
//
// Names were derived from the app and branch — values two deployments can share —
// so a name-based sweep could remove a bystander's containers. Selection is
// strictly by ownership label and this deployment's own image repository.
func TestPurgeScript_SelectsByOwnershipNotName(t *testing.T) {
	s := purgeScript(purgeSpec())
	for _, want := range []string{
		`--filter "label=com.docker.compose.project=$DEPLOYMENT_ID"`,
		`--filter "label=mountabo.deployment=$DEPLOYMENT_ID"`,
		`--filter=reference='mountabo/mtb-aaaaaaaaaaaa:*'`,
		`--filter=reference='mountabo/mtb-aaaaaaaaaaaa-*:*'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("purge script missing %q:\n%s", want, s)
		}
	}
	// Nothing may be selected by an app name or a branch.
	for _, forbidden := range []string{"shop-main", "$proj", "reference='mountabo/shop"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("purge still selects on %q, which two deployments can share", forbidden)
		}
	}
	// Images are untagged, never forced: an image can carry other tags or back
	// another deployment's containers, and docker refusing is the correct outcome.
	if strings.Contains(s, "docker rmi -f") {
		t.Errorf("purge must not force image removal:\n%s", s)
	}
	// And never host-wide.
	if strings.Contains(s, "image prune") || strings.Contains(s, "system prune") {
		t.Errorf("purge must never prune host-wide:\n%s", s)
	}
}

// TestPurgeScript_VolumesAreOptIn covers the destructive choice. Named volumes
// hold databases and uploads, so a default that removed them would make an
// ordinary delete unrecoverable.
func TestPurgeScript_VolumesAreOptIn(t *testing.T) {
	off := purgeScript(purgeSpec())
	if strings.Contains(off, "docker volume rm") {
		t.Errorf("volumes must not be removed by default:\n%s", off)
	}
	if !strings.Contains(off, "kept: named volumes") {
		t.Errorf("the journal should say the volumes were kept:\n%s", off)
	}

	spec := purgeSpec()
	spec.RemoveVolumes = true
	on := purgeScript(spec)
	if !strings.Contains(on, "docker volume rm") {
		t.Errorf("volumes should be removed when asked:\n%s", on)
	}
	if !strings.Contains(on, `--filter "label=com.docker.compose.project=mtb-aaaaaaaaaaaa"`) {
		t.Errorf("volume removal must be scoped to this deployment:\n%s", on)
	}
}

// TestPurgeScript_DeployKeyIsOptIn covers a resource shared across a repo's
// environments. Removing it while a sibling still deploys would break that
// deployment's clone.
func TestPurgeScript_DeployKeyIsOptIn(t *testing.T) {
	kept := purgeScript(purgeSpec())
	if strings.Contains(kept, "rm -f \"$key\"") {
		t.Errorf("the deploy key must be kept while a sibling environment exists:\n%s", kept)
	}

	spec := purgeSpec()
	spec.RemoveDeployKey = true
	last := purgeScript(spec)
	if !strings.Contains(last, "mountabo_deploy_acme_shop") {
		t.Errorf("the deploy key should be removed when nothing else needs it:\n%s", last)
	}
}

// TestPurgeScript_KeepsTheTombstone is what stops a queued run recreating a
// deleted deployment. The state document and lock must survive the purge.
func TestPurgeScript_KeepsTheTombstone(t *testing.T) {
	s := purgeScript(purgeSpec())
	if strings.Contains(s, `rm -rf "$STATE_DIR"`) || strings.Contains(s, "locks/$DEPLOYMENT_ID.lock\"") {
		t.Errorf("the tombstone and lock must survive a purge:\n%s", s)
	}
	if !strings.Contains(s, `.lifecycle = "deleted"`) {
		t.Errorf("the purge should mark the deployment deleted:\n%s", s)
	}
}

// runPurgePaths exercises the path-validation helper against a real filesystem.
// String-matching cannot answer whether realpath, the containment check and the
// ownership test actually stop a bad path — only running them can.
func runPurgePaths(t *testing.T, setup func(dir string), target func(dir string) string, root func(dir string) string) (journal string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	setup(dir)

	// Only the path helper is exercised, with the docker section skipped.
	full := purgeScript(purgeSpec())
	start := strings.Index(full, "purge_dir() {")
	end := strings.Index(full, `purge_dir "deploy directory"`)
	if start < 0 || end < 0 {
		t.Fatalf("could not isolate the path helper:\n%s", full)
	}
	script := "#!/usr/bin/env bash\nset -uo pipefail\nJOURNAL=" + filepath.Join(dir, "journal") + "\n: > \"$JOURNAL\"\n" +
		full[start:end] +
		"purge_dir \"target\" " + root(dir) + " " + target(dir) + "\ncat \"$JOURNAL\"\n"

	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := exec.Command(bash, path).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	return string(out)
}

// TestPurgePaths_RefusesToEscapeTheDeployRoot is the guard that matters most,
// because the purge runs as root. A textual "starts with" check is not enough:
// a symlink or a ".." can point anywhere.
func TestPurgePaths_RefusesToEscapeTheDeployRoot(t *testing.T) {
	// A symlink inside the root pointing outside it.
	journal := runPurgePaths(t,
		func(dir string) {
			outside := filepath.Join(dir, "outside")
			_ = os.MkdirAll(outside, 0o755)
			_ = os.MkdirAll(filepath.Join(dir, "root"), 0o755)
			_ = os.Symlink(outside, filepath.Join(dir, "root", "escape"))
		},
		func(dir string) string { return filepath.Join(dir, "root", "escape") },
		func(dir string) string { return filepath.Join(dir, "root") },
	)
	if !strings.Contains(journal, "INCOMPLETE") || !strings.Contains(journal, "outside") {
		t.Errorf("a symlink escaping the deploy root should be refused:\n%s", journal)
	}

	// A traversal out of the root.
	journal = runPurgePaths(t,
		func(dir string) {
			_ = os.MkdirAll(filepath.Join(dir, "root"), 0o755)
			_ = os.MkdirAll(filepath.Join(dir, "sibling"), 0o755)
		},
		func(dir string) string { return filepath.Join(dir, "root", "..", "sibling") },
		func(dir string) string { return filepath.Join(dir, "root") },
	)
	if !strings.Contains(journal, "INCOMPLETE") {
		t.Errorf("a traversal out of the deploy root should be refused:\n%s", journal)
	}
}

func TestPurgePaths_RemovesAContainedDirectory(t *testing.T) {
	journal := runPurgePaths(t,
		func(dir string) { _ = os.MkdirAll(filepath.Join(dir, "root", "mtb-x"), 0o755) },
		func(dir string) string { return filepath.Join(dir, "root", "mtb-x") },
		func(dir string) string { return filepath.Join(dir, "root") },
	)
	if !strings.Contains(journal, "removed: target") {
		t.Errorf("a contained, owned directory should be removed:\n%s", journal)
	}
}

func TestPurgePaths_TreatsAnAbsentTargetAsDone(t *testing.T) {
	journal := runPurgePaths(t,
		func(dir string) { _ = os.MkdirAll(filepath.Join(dir, "root"), 0o755) },
		func(dir string) string { return filepath.Join(dir, "root", "never-existed") },
		func(dir string) string { return filepath.Join(dir, "root") },
	)
	if !strings.Contains(journal, "removed: target (absent)") {
		t.Errorf("an absent target is already gone, not a failure:\n%s", journal)
	}
}

func TestPurgePaths_RefusesRootAndEmpty(t *testing.T) {
	journal := runPurgePaths(t,
		func(_ string) {},
		func(_ string) string { return "/" },
		func(_ string) string { return "/" },
	)
	if !strings.Contains(journal, "INCOMPLETE") {
		t.Errorf("removing / must be refused outright:\n%s", journal)
	}
}
