package workflow

import (
	"strings"
	"testing"
)

// TestIdentity_TwoEnvironmentsShareNothing is the generator half of the
// "deleting one deployment cannot affect another" property — and of the wider
// rule that two deployments must not collide at all.
//
// The collision it guards against was real and total. Every server-side name was
// built from the app and the branch: the compose project ("<app>-<branch>"), the
// container ("<app>-<branch>"), the image repository ("mountabo/<app>"), the
// checkout ("$DEPLOY_DIR/<branch>") and the staging directory
// ("/tmp/<app>-deploy"). Two environments of one repo deploying the same branch
// therefore shared their containers, their project-scoped volumes, their image
// tags and their working tree — so deploying staging replaced production.
func TestIdentity_TwoEnvironmentsShareNothing(t *testing.T) {
	// The realistic worst case: same app, same repo, same branch, same server.
	prod := base()
	prod.Environment = "production"
	prod.DeploymentID = "mtb-111111111111"

	staging := base()
	staging.Environment = "staging"
	staging.DeploymentID = "mtb-222222222222"

	for _, strategy := range []Strategy{Compose, Docker} {
		prod.Strategy, staging.Strategy = strategy, strategy

		for name, pair := range map[string][2]string{
			"committed script":  {ScriptPath(prod), ScriptPath(staging)},
			"staging directory": {StagingDir(prod), StagingDir(staging)},
			"image repository":  {prod.imageRepo(), staging.imageRepo()},
			"ownership labels":  {prod.ownershipLabels(), staging.ownershipLabels()},
		} {
			if pair[0] == pair[1] {
				t.Errorf("%s/%s: both environments share %q", strategy, name, pair[0])
			}
		}

		// And in the generated script itself: the checkout, compose project and
		// container name must all differ.
		p, s := DeployScript(prod), DeployScript(staging)
		if p == s {
			t.Errorf("%s: both environments generate an identical script", strategy)
		}
		for _, marker := range []string{
			`DEPLOYMENT_ID="mtb-111111111111"`,
			`DEPLOY_DIR="${DEPLOY_DIR:-/opt/shop}/$DEPLOYMENT_ID"`,
		} {
			if !strings.Contains(p, marker) {
				t.Errorf("%s: production script missing %q:\n%s", strategy, marker, p)
			}
		}
		if strings.Contains(p, "mtb-222222222222") {
			t.Errorf("%s: production script references staging's identity:\n%s", strategy, p)
		}
	}
}

// TestIdentity_NamesDeriveFromTheIdentityNotTheApp guards against a name being
// rebuilt from the app or branch. Those are precisely the values two deployments
// can share, so any resource named from them reintroduces the collision.
func TestIdentity_NamesDeriveFromTheIdentityNotTheApp(t *testing.T) {
	c := base()
	c.Strategy = Compose
	compose := DeployScript(c)
	c.Strategy = Docker
	docker := DeployScript(c)

	for _, want := range []string{
		`export COMPOSE_PROJECT_NAME="` + testID + `"`,
		`mountabo.deployment=` + testID,
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose script missing %q:\n%s", want, compose)
		}
	}
	if !strings.Contains(docker, `CONTAINER="`+testID+`"`) {
		t.Errorf("docker container should be named for the deployment:\n%s", docker)
	}
	// The old "<app>-<branch>" convention must not survive anywhere.
	for _, forbidden := range []string{`COMPOSE_PROJECT_NAME='shop-main'`, `CONTAINER="shop-$BRANCH"`, `/tmp/shop-deploy`} {
		for _, s := range []string{compose, docker} {
			if strings.Contains(s, forbidden) {
				t.Errorf("script still uses the shared name %q:\n%s", forbidden, s)
			}
		}
	}
}

// TestIdentity_LabelsCarryTheFullKey checks the labels teardown selects on carry
// enough to identify the deployment for a human reading `docker inspect`, not
// just the opaque id.
func TestIdentity_LabelsCarryTheFullKey(t *testing.T) {
	c := base()
	c.Environment = "production"
	labels := c.ownershipLabels()
	for _, want := range []string{
		"mountabo.deployment=" + testID,
		"mountabo.owner=acme",
		"mountabo.repo=shop",
		"mountabo.environment=production",
	} {
		if !strings.Contains(labels, want) {
			t.Errorf("labels missing %q: %s", want, labels)
		}
	}
}
