package ssh

import (
	"strings"
	"testing"
)

// TestTeardownScript_CleansContainersImagesAndTarball guards that a teardown
// removes not just the app's containers (all project-name variants + plain
// docker-run names) but also the images mountabo built + loaded for it and the
// staged image tarball — otherwise offloaded images would leak on every delete.
func TestTeardownScript_CleansContainersImagesAndTarball(t *testing.T) {
	s := teardownScript("shop", "main")
	for _, want := range []string{
		"label=com.docker.compose.project=$proj", // container removal (unchanged)
		"docker rm -f",                           // plain docker-run containers
		"docker rmi -f",                          // built images
		`--filter=reference='mountabo/shop:*'`,   // docker strategy tag
		`--filter=reference='mountabo/shop-*:*'`, // compose per-service tags
		"rm -rf '/tmp/shop-deploy'",              // staged tarball dir
	} {
		if !strings.Contains(s, want) {
			t.Errorf("teardown script missing %q:\n%s", want, s)
		}
	}
}

// TestTeardownScript_LowercasesImageRepo guards that image cleanup uses the
// lowercased repo name, matching the tags the workflow builds (docker repository
// names must be lowercase), even when the app name has uppercase letters.
func TestTeardownScript_LowercasesImageRepo(t *testing.T) {
	s := teardownScript("MyApp", "main")
	if !strings.Contains(s, `--filter=reference='mountabo/myapp:*'`) {
		t.Errorf("teardown image ref should be lowercased:\n%s", s)
	}
}
