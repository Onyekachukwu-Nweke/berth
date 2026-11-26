package workflow

import (
	"fmt"
	"strings"
)

// bundleFormatVersion is the on-disk shape of a release bundle. It is recorded in
// every manifest so a future script can tell whether it understands a bundle it
// did not write — which is the whole reason each bundle also carries the runner
// that produced it.
const bundleFormatVersion = 1

// ArtifactIDExpr is the GitHub Actions expression that names one BUILD.
//
// It is deliberately not the commit sha. The same commit rebuilt with different
// build arguments, a moved base image or newly resolved dependencies produces a
// different image, and a secret change triggers exactly that rebuild — so a
// commit-keyed tag is overwritten in place and "the artifact that was running"
// stops being recoverable. The run number (plus the attempt on a re-run) makes
// every build of an environment distinct; the environment and the first
// characters of the sha make it readable. Actions expressions can't cut a
// string, so the workflow's first step computes it (see nameBuildStep) and
// later steps read it from env.
const ArtifactIDExpr = "${{ env.MOUNTABO_ARTIFACT_ID }}"

// shortSHALen is how much of the commit sha goes into a build's name.
const shortSHALen = 5

// nameBuildStep sets MOUNTABO_ARTIFACT_ID for the rest of the job:
// <environment>-<run number>[.<attempt>]-<first characters of the sha>, e.g.
// development-42-9f3c2, or development-42.2-9f3c2 for a re-run of run 42.
func nameBuildStep(c Config) string {
	return fmt.Sprintf(`      - name: Name this build
        run: |
          run="$GITHUB_RUN_NUMBER"
          [ "$GITHUB_RUN_ATTEMPT" = 1 ] || run="$run.$GITHUB_RUN_ATTEMPT"
          echo "MOUNTABO_ARTIFACT_ID=%s-${run}-${GITHUB_SHA::%d}" >> "$GITHUB_ENV"

`, c.environment(), shortSHALen)
}

// OperationIDExpr is the GitHub Actions expression that names one push-triggered
// OPERATION.
//
// mountabo cannot mint an id for an ordinary push: it is not running when the
// push happens. Deriving it from the repository and the run makes it stable and
// idempotent — re-reading the same run reconciles to the same operation instead
// of appending a second record. Operations mountabo starts itself supply their
// own id.
const OperationIDExpr = "${{ github.repository_id }}-${{ github.run_id }}-${{ github.run_attempt }}"

// stateHelpers are the shell functions that read and write the deployment's state
// document. The document is the single record of which artifact is active, which
// preceded it, and which is in flight.
//
// It is written whole, to a temp file, then renamed. rename(2) within a
// filesystem is atomic, so a crash mid-write leaves the previous document intact
// rather than a truncated one. Separate pointer files could tear against each
// other; one document cannot.
//
// generation increments on every write so a reader can tell the document changed
// underneath it. updated_at is for humans reading the file on the box.
const stateHelpers = `
# --- mountabo: release state --------------------------------------------------
mountabo_state_read() {
  if [ -f "$STATE_FILE" ]; then cat "$STATE_FILE"; else echo '{}'; fi
}

# mountabo_state_get FILTER -> the value, or "" when absent.
mountabo_state_get() {
  mountabo_state_read | jq -r "$1 // empty" 2>/dev/null || true
}

# mountabo_state_write FILTER [--arg k v ...]
# Writes the whole document via a temp file + rename, which is atomic within a
# filesystem: a crash cannot leave a half-written state document behind.
mountabo_state_write() {
  __filter="$1"; shift
  __tmp="$STATE_FILE.tmp.$$"
  if mountabo_state_read | jq -c \
      --arg __now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --arg __did "$DEPLOYMENT_ID" \
      "$@" \
      "$__filter | .deployment_id = \$__did | .updated_at = \$__now | .generation = ((.generation // 0) + 1)" \
      > "$__tmp"; then
    mv -f "$__tmp" "$STATE_FILE"
  else
    rm -f "$__tmp"
    echo "warning: could not update deployment state" >&2
  fi
}
`

// artifactIdentityBlock names this build, and is emitted immediately after the
// lock preamble — before anything that references the artifact.
//
// It has to come that early because the generated compose override stamps the
// artifact label onto every service, and that override is written well before
// the release bundle. Defining the id inside the bundle step instead left it
// unbound at the point of first use, which under "set -u" aborts the deploy.
const artifactIdentityBlock = `
# --- mountabo: this operation's identity --------------------------------------
# The artifact names one BUILD, not one commit: the same commit rebuilt with
# different build arguments or a moved base image is a different image, so a
# commit-keyed name would be overwritten in place and the artifact that had been
# running would stop being recoverable.
#
# A rollback names an artifact that already exists rather than minting one. It is
# the same script either way — one code path, so a rollback can never diverge
# from the deploy it is undoing — with only its inputs switched.
REL_ROOT="$MOUNTABO_STATE/releases/$DEPLOYMENT_ID"
if [ "$MOUNTABO_MODE" = rollback ]; then
  ARTIFACT_ID="${MOUNTABO_ARTIFACT:?MOUNTABO_ARTIFACT must name a retained release}"
  BUNDLE="$REL_ROOT/$ARTIFACT_ID"
  if [ ! -d "$BUNDLE" ]; then
    echo "error: release $ARTIFACT_ID is no longer retained on this server" >&2
    echo "       mountabo keeps the active release and the one before it; pick a newer release" >&2
    exit 1
  fi
  echo "=== rolling back to $ARTIFACT_ID (no rebuild) ==="
else
  ARTIFACT_ID="${MOUNTABO_ARTIFACT_ID:?MOUNTABO_ARTIFACT_ID must be set by the workflow}"
  BUNDLE="$REL_ROOT/$ARTIFACT_ID"
fi
OPERATION_ID="${MOUNTABO_OPERATION_ID:-}"
`

// rollbackKind records why the state moved, so release history can tell a
// rollback from an ordinary deploy.
const rollbackKindBlock = `MOUNTABO_KIND="${MOUNTABO_KIND:-deploy}"
`

// bundleBlock writes the release bundle and marks the artifact as the candidate,
// BEFORE anything touches the running containers.
//
// Ordering is the point. Tagging or recording a release after the swap leaves a
// window in which some containers have already changed but no complete record of
// what is now running exists — and that is exactly when a recovery record is
// needed. The bundle is therefore complete while the previous release is still
// serving, and only then is the swap attempted.
//
// The bundle carries the runner that produced it. A rollback executes THAT
// script, not today's committed one, so an artifact written by an older format
// is never handed to a script that does not understand it.
func bundleBlock(c Config) string {
	extra := ""
	if c.strategy() == Compose {
		// The image pins the runner generated. This is what re-points the stack at
		// these exact images on a rollback, with no rebuild.
		extra = `if [ -f mountabo.image.override.yml ]; then
  cp mountabo.image.override.yml "$BUNDLE/image.override.yml"
fi
`
	}
	return fmt.Sprintf(`
# --- mountabo: record this build before touching anything running -------------
# Skipped on rollback: that artifact's bundle was written when it was built, and
# rewriting it would overwrite the record of what it actually is.
if [ "$MOUNTABO_MODE" = deploy ]; then
mkdir -p "$BUNDLE"

# The images this release actually runs, by immutable id as well as by tag: a tag
# can be moved, an image id cannot, so the id is what proves a rollback started
# the same bytes that were running before.
mountabo_image_rows() {
  for ref in ${loaded:-}; do
    id="$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null || echo "")"
    jq -n --arg ref "$ref" --arg id "$id" '{ref: $ref, id: $id}'
  done | jq -s '.'
}

jq -n \
  --argjson fv %[1]d \
  --arg artifact "$ARTIFACT_ID" \
  --arg operation "$OPERATION_ID" \
  --arg commit "%[2]s" \
  --arg strategy "%[3]s" \
  --arg deployment "$DEPLOYMENT_ID" \
  --arg dir "$PWD" \
  --arg at "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" \
  --argjson images "$(mountabo_image_rows)" \
  '{format_version: $fv, artifact_id: $artifact, operation_id: $operation,
    commit: $commit, strategy: $strategy, deployment_id: $deployment,
    working_dir: $dir, at: $at, images: $images}' > "$BUNDLE/manifest.json"

# Keep the script that produced this bundle beside it. A rollback runs this copy,
# so a bundle written by an older format is never interpreted by a newer script.
cp "$0" "$BUNDLE/runner.sh" 2>/dev/null || true
chmod 600 "$BUNDLE/manifest.json" 2>/dev/null || true
%[4]s
echo "recorded release $ARTIFACT_ID"
fi
mountabo_state_write '.lifecycle = "deploying" | .candidate = $a | .incarnation = ($inc | tonumber)' \
  --arg a "$ARTIFACT_ID" --arg inc "$EXPECT_INCARNATION"
`, bundleFormatVersion, "${MOUNTABO_COMMIT:-unknown}", c.strategy(), extra)
}

// verifyAndPromoteBlock confirms the intended artifact is the one actually
// serving, then commits the release state.
//
// Probing a port is not enough on its own: if the swap silently failed, the
// PREVIOUS container is still answering and a plain health check passes while
// reporting the new release as live. So the running containers' mountabo.artifact
// label is compared against the candidate first, and only both together promote.
//
// Promotion is where the retention pointers move, and the rule that matters is
// that active and previous must never converge — collapsing them would make the
// release just replaced eligible for pruning and destroy the rollback target.
func verifyAndPromoteBlock(_ Config) string {
	return `
# --- mountabo: verify what is actually serving, then commit the release -------
mountabo_running_artifacts() {
  ids="$(docker ps -q --filter "label=mountabo.deployment=$DEPLOYMENT_ID" 2>/dev/null || true)"
  [ -n "$ids" ] || return 0
  for cid in $ids; do
    docker inspect -f '{{ index .Config.Labels "mountabo.artifact" }}' "$cid" 2>/dev/null || true
  done
}

serving="$(mountabo_running_artifacts | sort -u | grep -v '^$' || true)"
if [ -z "$serving" ]; then
  echo "error: no running container carries this deployment's label; the swap did not take" >&2
  mountabo_state_write '.lifecycle = "degraded" | .running = "unknown"'
  exit 1
fi
if [ "$serving" != "$ARTIFACT_ID" ]; then
  echo "error: expected artifact $ARTIFACT_ID to be serving, but found: $serving" >&2
  echo "       the previous release may still be up; mountabo will show this as degraded" >&2
  mountabo_state_write '.lifecycle = "degraded" | .running = "unknown"'
  exit 1
fi
echo "verified: $ARTIFACT_ID is serving"

# Commit the release. A redeploy of the artifact that is already active must NOT
# move previous onto it: that would leave active and previous identical and make
# the release before it prunable, destroying the rollback target.
__prev="$(mountabo_state_get '.active')"
if [ -n "$__prev" ] && [ "$__prev" != "$ARTIFACT_ID" ]; then
  mountabo_state_write '.previous = $p | .active = $a | .candidate = null | .running = $a | .lifecycle = "active" | .kind = $k' \
    --arg p "$__prev" --arg a "$ARTIFACT_ID" --arg k "$MOUNTABO_KIND"
else
  mountabo_state_write '.active = $a | .candidate = null | .running = $a | .lifecycle = "active" | .kind = $k' \
    --arg a "$ARTIFACT_ID" --arg k "$MOUNTABO_KIND"
fi
`
}

// pruneBlock removes release bundles that are no longer recovery points, taking
// each bundle and its image tags together.
//
// It keeps exactly the artifacts the state document names: active, previous, and
// any in-flight candidate. Keeping "the newest N directories" instead would be
// wrong — a failed deploy or a rollback breaks that ordering, and the pair that
// matters is not always the newest pair.
//
// Two things it deliberately does not do. It never prunes host-wide: an image can
// carry other tags or back another deployment's containers, and a label does not
// make it exclusively owned, so only tags this deployment's own manifests name
// are removed. And it never removes an image without its bundle, or the reverse —
// an image whose env and invocation snapshot has been deleted is not recoverable,
// so keeping one without the other is worse than keeping neither.
const pruneBlock = `
# --- mountabo: keep the recovery points, retire the rest ----------------------
mountabo_prune_releases() {
  [ -d "$REL_ROOT" ] || return 0
  keep="$(mountabo_state_read | jq -r '[.active, .previous, .candidate] | map(select(. != null and . != "")) | .[]' 2>/dev/null || true)"
  for d in "$REL_ROOT"/*; do
    [ -d "$d" ] || continue
    a="$(basename "$d")"
    if printf '%s\n' "$keep" | grep -qxF "$a"; then continue; fi
    if [ -f "$d/manifest.json" ]; then
      jq -r '.images[]?.ref // empty' "$d/manifest.json" 2>/dev/null | while IFS= read -r ref; do
        [ -n "$ref" ] || continue
        # Untags only. docker refuses while a container still uses the image, and
        # that refusal is correct: it is another deployment's, or still running.
        docker rmi "$ref" >/dev/null 2>&1 || true
      done
    fi
    rm -rf "$d"
    echo "retired release $a"
  done
}
mountabo_prune_releases
`

// artifactLabelArgs renders the ownership labels plus the per-build artifact
// label as docker run arguments. The artifact label is what lets readiness prove
// the intended release is the one serving, rather than trusting that the swap
// worked.
func (c Config) artifactLabelArgs() []string {
	out := c.ownershipLabelArgs()
	return append(out, `"mountabo.artifact=$ARTIFACT_ID"`)
}

// allLabels is the ownership set plus the artifact label, space separated, for
// the generated compose override.
func (c Config) allLabels() string {
	return c.ownershipLabels() + " mountabo.artifact=$ARTIFACT_ID"
}

// commitEnvLine forwards the deployed commit into the script so the manifest can
// record it. It is metadata: the artifact id, not the commit, identifies a build.
func commitEnvLine() string {
	return strings.TrimSpace("MOUNTABO_COMMIT: ${{ github.sha }}")
}

// indentShell would indent a nested fragment for readability, but it must not:
// several fragments contain here-documents, and indenting the body moves the
// closing delimiter off column zero so it no longer terminates. The bash -n
// harness catches that immediately, which is how this was found. Nested blocks
// are therefore emitted at their original indentation, and the mode guards are
// commented instead.
func indentShell(s string) string {
	return strings.TrimRight(s, "\n")
}
