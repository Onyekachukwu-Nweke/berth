package ssh

import (
	"context"
	"fmt"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

var _ usecase.ContainerTeardown = (*Client)(nil)

// teardownScriptFmt removes the resources a deployment owns, selecting them by
// OWNERSHIP rather than by name.
//
// Selecting by name cannot work: names were built from the app and branch, which
// two deployments can share, so a name-based sweep could remove another
// deployment's containers. Every resource mountabo creates now carries
// "mountabo.deployment=<id>" (and compose additionally sets
// "com.docker.compose.project=<id>", which is the same id), so both filters
// select exactly one deployment and nothing else.
//
//	%[1]s = the deployment id, single-quoted
//	%[2]s = image reference for the docker strategy   (mountabo/<id>:*)
//	%[3]s = image reference for the compose strategy  (mountabo/<id>-*:*)
//	%[4]s = the staging directory                     (/tmp/<id>-deploy)
//
// Best-effort by design: a server without docker, or with no matching resource,
// is a clean no-op rather than a failure. The leading `command -v docker` guard
// exits 0 when docker is absent so the caller never sees an error.
const teardownScriptFmt = `if ! command -v docker >/dev/null 2>&1; then
  echo "docker is not installed on this server"
  exit 0
fi
# Containers, by ownership label. Both filters name the same deployment: compose
# stamps com.docker.compose.project, and every service also carries
# mountabo.deployment from the generated override (as does a docker-run container).
ids=$(docker ps -aq --filter "label=com.docker.compose.project=%[1]s" --filter "label=mountabo.deployment=%[1]s" | sort -u)
[ -z "$ids" ] || echo "$ids" | xargs docker rm -f
# Images mountabo built and loaded for this deployment. They are tagged per
# commit sha under a per-deployment repository, so they are not dangling and a
# plain prune would leave them behind. Two reference filters cover both
# strategies; docker ORs them.
ids=$(docker images --filter=reference=%[2]s --filter=reference=%[3]s --format '{{.ID}}' | sort -u)
[ -z "$ids" ] || echo "$ids" | xargs docker rmi -f 2>/dev/null || true
# The staged image tarball and deploy script for this deployment.
rm -rf %[4]s 2>/dev/null || true
echo "torn down %[1]s"
`

// RemoveApp stops and removes the deployment's containers on the server over
// SSH, plus the images mountabo built for it and its staging directory.
//
// It selects strictly by the deployment's ownership labels, so tearing one
// deployment down cannot touch another that happens to share an app name, a
// branch, or a server. Best-effort: docker missing on the box or no matching
// resource is NOT an error, only a connection or session failure is. It connects
// as the mountabo user and runs without sudo — that user is in the docker group
// from bootstrap.
func (c *Client) RemoveApp(ctx context.Context, t usecase.SSHTarget, deploymentID string) error {
	if _, err := c.runOutput(ctx, t, teardownScript(deploymentID)); err != nil {
		return fmt.Errorf("remove deployment %s: %w", deploymentID, err)
	}
	return nil
}

// teardownScript renders the teardown shell for one deployment. Pure text, so it
// is unit-testable without SSH. Every interpolated value is single-quote-escaped.
func teardownScript(deploymentID string) string {
	id := shellSingleQuote(deploymentID)
	repo := "mountabo/" + deploymentID
	return fmt.Sprintf(teardownScriptFmt,
		id,
		shellSingleQuote(repo+":*"),
		shellSingleQuote(repo+"-*:*"),
		shellSingleQuote("/tmp/"+deploymentID+"-deploy"),
	)
}
