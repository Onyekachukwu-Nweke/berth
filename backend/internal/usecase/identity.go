package usecase

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// deploymentIDPrefix marks a canonical deployment id so it is recognisable in
// container names, image references and paths on a server that may also carry
// resources mountabo did not create.
const deploymentIDPrefix = "mtb-"

// DeriveDeploymentID returns the canonical server-side identity for a
// deployment, derived from its canonical key (owner, repo, environment) and
// nothing else.
//
// Every server-side resource is named from this: the compose project, container
// names, the image namespace, the checkout path, the lock, the state document
// and the release bundles — plus the ownership labels teardown selects on. That
// is what makes two environments of one repo, or two repos of one owner,
// incapable of colliding. The previous convention keyed those names on
// "<app>-<branch>", so two environments sharing a branch shared a compose
// project, a lock and a container set.
//
// The app name is deliberately NOT an input: renaming the app would otherwise
// move a live deployment's identity even though its canonical key never changed.
// For the same reason the id is persisted on the deployment record the first
// time it is computed and read back thereafter — see Deployment.DeploymentID —
// so even a future change to this derivation cannot re-home existing resources.
//
// The result is [a-z0-9-] only, so it is directly usable as a docker tag
// component, a compose project name, a file name and a path segment.
func DeriveDeploymentID(owner, repo, environment string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(owner) + "\x00" + strings.TrimSpace(repo) + "\x00" + strings.TrimSpace(environment)))
	return deploymentIDPrefix + hex.EncodeToString(sum[:6])
}

// DeploymentIDFor returns the deployment's identity: the stored one, which names
// the resources that actually exist on the server.
//
// It derives the id only when the record carries none, which can happen for a
// deployment recorded but never deployed. It is NOT a compatibility path — a
// stored id and a derived one are the same value for the same key, and the
// stored one is authoritative precisely so a change to the derivation can never
// re-home live resources.
func DeploymentIDFor(d Deployment) string {
	if id := strings.TrimSpace(d.DeploymentID); id != "" {
		return id
	}
	return DeriveDeploymentID(d.Owner, d.Repo, d.Environment)
}

// Slug folds an arbitrary string into the character set docker accepts for an
// image repository and tag component: lowercase letters, digits, and the
// separators "." "_" "-". Anything else becomes "-".
//
// It exists because branch names reach docker tags. The previous generator built
// IMAGE="<app>:$BRANCH" directly, so any branch containing "/" — "feature/x" —
// produced an illegal tag and the docker strategy simply could not deploy it.
func Slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}
