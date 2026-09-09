package usecase

import (
	"regexp"
	"strings"
	"testing"
)

// The id must be usable verbatim as a docker tag component, a compose project
// name, a file name and a path segment, so it may only contain [a-z0-9-].
var idShape = regexp.MustCompile(`^mtb-[0-9a-f]{12}$`)

func TestDeriveDeploymentID_DistinguishesEveryPartOfTheKey(t *testing.T) {
	base := DeriveDeploymentID("acme", "shop", "production")
	if !idShape.MatchString(base) {
		t.Fatalf("id %q is not a safe identifier", base)
	}

	// The defect this replaces: two environments of one repo on the same branch
	// previously shared "<app>-<branch>", so they shared a compose project, a
	// lock and a container set. Each component of the key must move the id.
	for name, other := range map[string]string{
		"environment differs": DeriveDeploymentID("acme", "shop", "staging"),
		"repo differs":        DeriveDeploymentID("acme", "till", "production"),
		"owner differs":       DeriveDeploymentID("other", "shop", "production"),
	} {
		if other == base {
			t.Errorf("%s: id collided with %q", name, base)
		}
	}

	if again := DeriveDeploymentID("acme", "shop", "production"); again != base {
		t.Errorf("derivation is not stable: %q then %q", base, again)
	}
	// Surrounding whitespace must not fork a live deployment's identity.
	if trimmed := DeriveDeploymentID(" acme ", "shop", "production\n"); trimmed != base {
		t.Errorf("whitespace changed the id: %q vs %q", trimmed, base)
	}
}

func TestDeriveDeploymentID_IgnoresAppAndBranch(t *testing.T) {
	// Renaming the app must not move the identity of resources already on the
	// server, which is why neither app nor branch is an input.
	want := DeriveDeploymentID("acme", "shop", "production")
	d := Deployment{Owner: "acme", Repo: "shop", Environment: "production", App: "renamed", Branch: "other"}
	if got := DeploymentIDFor(d); got != want {
		t.Errorf("DeploymentIDFor derived %q, want %q", got, want)
	}
}

func TestDeploymentIDFor_PrefersTheStoredID(t *testing.T) {
	// A stored id wins over the derivation: it names the resources that actually
	// exist, so even changing the derivation later cannot re-home them.
	d := Deployment{Owner: "acme", Repo: "shop", Environment: "production", DeploymentID: "mtb-deadbeef0000"}
	if got := DeploymentIDFor(d); got != "mtb-deadbeef0000" {
		t.Errorf("stored id ignored: got %q", got)
	}
	// A record with no id yet (configured but never deployed) still resolves.
	fresh := Deployment{Owner: "acme", Repo: "shop", Environment: "production", DeploymentID: "  "}
	if got := DeploymentIDFor(fresh); got != DeriveDeploymentID("acme", "shop", "production") {
		t.Errorf("record without a stored id did not derive one: got %q", got)
	}
}

func TestSlug_ProducesLegalDockerTagComponents(t *testing.T) {
	// "feature/x" previously reached `docker build -t <app>:$BRANCH` verbatim,
	// which docker rejects — the docker strategy could not deploy such a branch.
	legal := regexp.MustCompile(`^[a-z0-9._-]+$`)
	for _, in := range []string{"feature/x", "Feature/Big Thing", "release~1", "MAIN", "ünïcode", "a/b/c"} {
		got := Slug(in)
		if !legal.MatchString(got) {
			t.Errorf("Slug(%q) = %q, which docker will not accept", in, got)
		}
		if strings.ContainsAny(got, "/ ") {
			t.Errorf("Slug(%q) = %q still contains a path or space character", in, got)
		}
	}
	if got := Slug("feature/x"); got != "feature-x" {
		t.Errorf("Slug(\"feature/x\") = %q, want \"feature-x\"", got)
	}
	// An all-illegal input must still yield a usable name rather than "".
	if got := Slug("///"); got == "" || !legal.MatchString(got) {
		t.Errorf("Slug(\"///\") = %q, want a usable fallback", got)
	}
}
