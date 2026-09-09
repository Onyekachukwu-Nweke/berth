package ssh

import (
	"context"
	"fmt"
	"io"

	"github.com/goodylili/mountabo/internal/usecase"
)

var (
	_ usecase.DeployScriptRunner = (*Client)(nil)
	_ usecase.ReleaseStateReader = (*Client)(nil)
)

// RunAsDeployUser pipes a script to the server and runs it as the connected
// (mountabo) user, streaming combined output to out.
//
// Unlike RunAsRoot it does not sudo. The deploy directory, the compose project
// and the release bundles all belong to the deploy user, and that user is in the
// docker group from bootstrap — running these as root would leave root-owned
// files in a tree the next deploy writes to as the deploy user, which then fails
// in a way that is hard to diagnose.
func (c *Client) RunAsDeployUser(ctx context.Context, t usecase.SSHTarget, script string, out io.Writer) error {
	if err := c.runScript(ctx, t, "bash -s", script, out); err != nil {
		return fmt.Errorf("run deploy script: %w", err)
	}
	return nil
}

// ReadReleaseState reads a deployment's state document.
//
// Read-only and bounded, and a missing document is an empty state rather than an
// error: a deployment that has not run since release tracking shipped simply has
// none yet, and failing there would stop the UI showing the deployment at all.
func (c *Client) ReadReleaseState(ctx context.Context, t usecase.SSHTarget, deploymentID string) (usecase.ReleaseState, error) {
	cmd := fmt.Sprintf(`f="$HOME/.mountabo/state/"%s".json"; [ -f "$f" ] && cat "$f" || true`,
		shellSingleQuote(deploymentID))
	out, err := c.runOutput(ctx, t, cmd)
	if err != nil {
		return usecase.ReleaseState{}, fmt.Errorf("read release state: %w", err)
	}
	return usecase.ParseReleaseState(out)
}
