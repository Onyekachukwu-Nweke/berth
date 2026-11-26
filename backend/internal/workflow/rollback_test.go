package workflow

import (
	"strings"
	"testing"
)

// TestRollback_NeverRebuilds is the defining property of a rollback: it must
// re-point to a retained artifact, not rebuild an old commit.
//
// Rebuilding is not the same thing and cannot be. Floating base images and
// unpinned dependencies mean a rebuilt "previous release" is not the artifact
// that was running, so a rollback that rebuilds does not restore what broke.
func TestRollback_NeverRebuilds(t *testing.T) {
	for _, strategy := range []Strategy{Compose, Docker} {
		c := base()
		c.Strategy = strategy
		s := DeployScript(c)

		// No build command may appear anywhere in the script — the runner builds,
		// the server never does.
		for _, forbidden := range []string{"docker build", "docker compose $COMPOSE_ARGS build", "compose build"} {
			if strings.Contains(s, forbidden) {
				t.Errorf("%s: script contains a build step (%q); the server must never build", strategy, forbidden)
			}
		}
		// And the compose activation must be explicitly --no-build.
		if strategy == Compose && !strings.Contains(s, "up -d --no-build") {
			t.Errorf("compose activation must pass --no-build:\n%s", s)
		}
	}
}

// TestRollback_TakesItsInputsFromTheBundle checks that a rollback resolves what
// to run from the target release's own recorded manifest and pins, rather than
// from whatever the workflow last shipped or the working tree currently says.
func TestRollback_TakesItsInputsFromTheBundle(t *testing.T) {
	c := base()
	c.Strategy = Compose
	compose := DeployScript(c)
	c.Strategy = Docker
	docker := DeployScript(c)

	if !strings.Contains(compose, `cp "$BUNDLE/image.override.yml" ./mountabo.image.override.yml`) {
		t.Errorf("compose rollback should pin from the bundle:\n%s", compose)
	}
	if !strings.Contains(docker, `jq -r '.images[0].ref // empty' "$BUNDLE/manifest.json"`) {
		t.Errorf("docker rollback should take its image from the bundle manifest:\n%s", docker)
	}
	// A bundle without recorded images cannot be rolled back to, and saying so is
	// better than starting something unintended.
	for name, s := range map[string]string{"compose": compose, "docker": docker} {
		if !strings.Contains(s, "cannot be rolled back to") {
			t.Errorf("%s: a bundle with no recorded image should be refused:\n%s", name, s)
		}
	}
}

// TestRollback_DoesNotDisturbTheWorkingTreeOrConfiguration pins two things a
// rollback must leave alone.
//
// The compose files: a rollback unpacks the copy kept with its release, and
// only a deploy records a new one. The .env: GitHub Actions secret values
// are write-only, so mountabo cannot reconstruct it — a rollback restores code,
// not secrets, and the file already on the box is the current configuration.
func TestRollback_DoesNotDisturbTheWorkingTreeOrConfiguration(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)

	guarded := func(needle string) bool {
		i := strings.Index(s, needle)
		if i < 0 {
			return false
		}
		// Walk back to the nearest mode guard and confirm it is the deploy one.
		before := s[:i]
		j := strings.LastIndex(before, `if [ "$MOUNTABO_MODE" = deploy ]; then`)
		k := strings.LastIndex(before, "\nfi\n")
		return j >= 0 && j > k
	}
	for _, needle := range []string{`cp "$COMPOSE_TAR" "$BUNDLE/compose.tar.gz"`, "wrote $written environment variable(s) to .env"} {
		if !guarded(needle) {
			t.Errorf("%q must run only on a deploy, not a rollback:\n%s", needle, s)
		}
	}
	// A rollback runs against the compose files kept with the target release,
	// not whatever the latest deploy left in the directory.
	if !strings.Contains(s, "if [ \"$MOUNTABO_MODE\" = rollback ]; then\n  COMPOSE_TAR=\"$BUNDLE/compose.tar.gz\"\nfi") {
		t.Errorf("rollback must unpack the release's own compose files:\n%s", s)
	}
}

// TestRollback_DoesNotRemoveOrphans covers a destructive surprise. On a deploy the
// compose file is the stack, so a service the operator deleted should go. On a
// rollback, a service that exists today but not in the target release would be
// stopped silently — that is not a rollback, it is data loss with a friendly name.
func TestRollback_DoesNotRemoveOrphans(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)

	i := strings.Index(s, `if [ "$MOUNTABO_MODE" = rollback ]; then`+"\n  docker compose $COMPOSE_ARGS up -d --no-build --force-recreate\n")
	if i < 0 {
		t.Fatalf("rollback activation not found or still passes --remove-orphans:\n%s", s)
	}
}

// TestRollback_ReusesOneCodePath guards the reason a rollback is expressed as a
// mode of the deploy script rather than a second script: two scripts drift, and a
// rollback that has drifted from the deploy it is undoing is worse than none.
func TestRollback_ReusesOneCodePath(t *testing.T) {
	c := base()
	c.Strategy = Compose
	s := DeployScript(c)

	// The activation, verification and promotion are emitted once, not twice.
	if n := strings.Count(s, "mountabo_running_artifacts()"); n != 1 {
		t.Errorf("verification is defined %d times; it must be shared by both modes", n)
	}
	if n := strings.Count(s, "up -d --no-build"); n != 2 {
		// Exactly two: the rollback arm and the deploy arm of one if.
		t.Errorf("expected one activation with two arms, found %d occurrences:\n%s", n, s)
	}
	if !strings.Contains(s, `ARTIFACT_ID="${MOUNTABO_ARTIFACT:?MOUNTABO_ARTIFACT must name a retained release}"`) {
		t.Errorf("rollback should name an existing artifact rather than mint one:\n%s", s)
	}
}
