package ssh

import (
	"strings"
	"testing"
)

// TestTeardownScript_SelectsByOwnershipNotName is the teardown half of the
// "deleting one deployment cannot affect another" property.
//
// Resources used to be selected by name, built from the app and branch — values
// two deployments can share, so a sweep could remove a bystander's containers.
// Selection is now strictly by the deployment's ownership labels and its own
// per-deployment image repository.
func TestTeardownScript_SelectsByOwnershipNotName(t *testing.T) {
	const id = "mtb-aaaaaaaaaaaa"
	s := teardownScript(id)

	for _, want := range []string{
		`--filter "label=com.docker.compose.project='` + id + `'"`, // compose stack
		`--filter "label=mountabo.deployment='` + id + `'"`,        // every mountabo resource
		"docker rm -f",
		"docker rmi -f",
		`--filter=reference='mountabo/` + id + `:*'`,   // docker strategy tags
		`--filter=reference='mountabo/` + id + `-*:*'`, // compose per-service tags
		`rm -rf '/tmp/` + id + `-deploy'`,              // staging directory
	} {
		if !strings.Contains(s, want) {
			t.Errorf("teardown script missing %q:\n%s", want, s)
		}
	}
}

// TestTeardownScript_NamesNothingButTheDeployment guards against a name-based
// filter creeping back in. Nothing in the script may mention an app or a branch:
// those are exactly the values that cannot distinguish two deployments.
func TestTeardownScript_NamesNothingButTheDeployment(t *testing.T) {
	s := teardownScript("mtb-aaaaaaaaaaaa")
	for _, forbidden := range []string{"shop", "main", "$proj", "$branch", "$app"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("teardown script still selects on %q, which two deployments can share:\n%s", forbidden, s)
		}
	}
}

// TestTeardownScript_EscapesTheIdentity keeps the id from breaking out of the
// shell quoting, even though ids are generated and constrained.
func TestTeardownScript_EscapesTheIdentity(t *testing.T) {
	s := teardownScript("mtb-a'b")
	if strings.Contains(s, "label=mountabo.deployment=mtb-a'b") {
		t.Errorf("identity was not shell-escaped:\n%s", s)
	}
}
