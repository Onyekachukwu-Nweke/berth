package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// The deployment identity is (owner, repo, environment): a repo can have several
// first-class environments (production, staging, ...), each with its own branch,
// server, domain and release history. branch is an attribute of the environment,
// not part of the key. deployment_id is the canonical name every server-side
// resource derives from (see usecase.DeriveDeploymentID); it is written once and
// never updated, because it names containers, images and paths that already exist.
// The schema lives in migrations/.

// DeploymentSQL persists configured deployments in Postgres and records an
// append-only event on every deploy, for durable tracking. It satisfies
// usecase.DeploymentStore.
type DeploymentSQL struct {
	db *sql.DB
}

var (
	_ usecase.DeploymentStore      = (*DeploymentSQL)(nil)
	_ usecase.ReleaseReader        = (*DeploymentSQL)(nil)
	_ usecase.DeploymentDeleter    = (*DeploymentSQL)(nil)
	_ usecase.DeploymentPortSetter = (*DeploymentSQL)(nil)
)

// NewDeploymentSQL returns a Postgres-backed deployment store over an open db.
func NewDeploymentSQL(db *sql.DB) *DeploymentSQL {
	return &DeploymentSQL{db: db}
}

// List returns the current deployments (one per target), oldest first.
func (s *DeploymentSQL) List() ([]usecase.Deployment, error) {
	rows, err := s.db.Query(`SELECT id, app, owner, repo, branch, environment, server_id, workflow_file, port, root_dir, deploy_config, deployment_id, created_at
		FROM deployments ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("query deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []usecase.Deployment
	for rows.Next() {
		var d usecase.Deployment
		var deployConfig []byte
		if err := rows.Scan(&d.ID, &d.App, &d.Owner, &d.Repo, &d.Branch, &d.Environment, &d.ServerID, &d.WorkflowFile, &d.Port, &d.RootDir, &deployConfig, &d.DeploymentID, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		if len(deployConfig) > 0 {
			var cfg usecase.DeployConfig
			if json.Unmarshal(deployConfig, &cfg) == nil {
				d.Config = &cfg
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Save upserts the deployment by owner+repo+environment, keeping the original id
// and first-seen created_at on re-deploy, and appends a deploy event for
// tracking. Both writes share one transaction so history never drifts from state.
func (s *DeploymentSQL) Save(d usecase.Deployment) error {
	createdAt := d.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deployConfig []byte // NULL when absent: a legacy record with no config
	if d.Config != nil {
		if b, merr := json.Marshal(d.Config); merr == nil {
			deployConfig = b
		}
	}

	// Keep the original id and created_at on conflict (first-deploy time), so
	// the deploy_events log carries the per-deploy history instead.
	// deployment_id is written once and never updated on conflict: it names the
	// containers, images, volumes and paths that already exist on the server, so a
	// re-deploy must not be able to move it. COALESCE-style CASE keeps the stored
	// value even if a caller passes an empty one.
	if _, err := tx.Exec(`INSERT INTO deployments
		(owner, repo, branch, id, app, environment, server_id, workflow_file, port, root_dir, deploy_config, deployment_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT(owner, repo, environment) DO UPDATE SET
			branch = excluded.branch,
			app = excluded.app,
			server_id = excluded.server_id,
			workflow_file = excluded.workflow_file,
			port = excluded.port,
			root_dir = excluded.root_dir,
			deploy_config = excluded.deploy_config,
			deployment_id = CASE WHEN deployments.deployment_id != '' THEN deployments.deployment_id ELSE excluded.deployment_id END`,
		d.Owner, d.Repo, d.Branch, d.ID, d.App, d.Environment, d.ServerID, d.WorkflowFile, d.Port, d.RootDir, deployConfig,
		usecase.DeploymentIDFor(d), createdAt.UTC()); err != nil {
		return fmt.Errorf("upsert deployment: %w", err)
	}

	// Link this release to the environment's previous one (NULL for the first), so
	// release history is a chain rollback can walk. Computed inside the same
	// transaction so a concurrent deploy cannot slot a release in between.
	var prevID sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(id) FROM deploy_events WHERE owner = $1 AND repo = $2 AND environment = $3`,
		d.Owner, d.Repo, d.Environment).Scan(&prevID); err != nil {
		return fmt.Errorf("find previous release: %w", err)
	}

	// A release is 'pending' until its GitHub Actions run completes; the terminal
	// status and run_url are overlaid live from the run at read time. image_ref
	// and health are provisioned but populated by later phases.
	if _, err := tx.Exec(`INSERT INTO deploy_events
		(owner, repo, branch, environment, server_id, app, at, commit_sha, actor, status, image_ref, health, run_url, prev_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', '', '', '', $10)`,
		d.Owner, d.Repo, d.Branch, d.Environment, d.ServerID, d.App, time.Now().UTC(), d.CommitSHA, d.Actor, prevID); err != nil {
		return fmt.Errorf("record release: %w", err)
	}
	return tx.Commit()
}

// SetPort updates a deployment's recorded published port (owner, repo,
// environment), without appending a release event — it records the port an
// operator declared (e.g. when attaching a domain) so the deployment and its
// domain agree on the port. It reports whether a row was updated; false means no
// such deployment (so the caller can 404 rather than report a phantom success).
func (s *DeploymentSQL) SetPort(owner, repo, environment string, port int) (bool, error) {
	res, err := s.db.Exec(`UPDATE deployments SET port = $1 WHERE owner = $2 AND repo = $3 AND environment = $4`,
		port, owner, repo, environment)
	if err != nil {
		return false, fmt.Errorf("set deployment port: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return affected > 0, nil
}

// Delete removes a tracked deployment and its entire append-only event history by
// its identity (owner, repo, environment), in one transaction so state and
// history never drift. It reports whether a deployment row was actually removed
// (false means no such deployment, so the caller can answer 404).
func (s *DeploymentSQL) Delete(owner, repo, environment string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Drop the history first, then the state, so a row never loses its state while
	// its events linger (the reverse would briefly orphan events on partial fail,
	// though the transaction makes both atomic regardless).
	if _, err := tx.Exec(`DELETE FROM deploy_events WHERE owner = $1 AND repo = $2 AND environment = $3`,
		owner, repo, environment); err != nil {
		return false, fmt.Errorf("delete deploy events: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM deployments WHERE owner = $1 AND repo = $2 AND environment = $3`,
		owner, repo, environment)
	if err != nil {
		return false, fmt.Errorf("delete deployment: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return affected > 0, nil
}

// Releases returns a target's most recent releases (newest first, capped to
// limit) from the append-only log, plus the total number ever recorded. Each
// release carries the durable fields captured at deploy time (commit sha, actor,
// environment, status, previous-release link); image_ref/health/run_url are
// provisioned but populated by later phases.
func (s *DeploymentSQL) Releases(owner, repo, environment string, limit int) ([]usecase.Release, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deploy_events WHERE owner = $1 AND repo = $2 AND environment = $3`,
		owner, repo, environment).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count releases: %w", err)
	}

	rows, err := s.db.Query(`SELECT id, at, environment, commit_sha, image_ref, status, health, actor, run_url, prev_event_id
		FROM deploy_events
		WHERE owner = $1 AND repo = $2 AND environment = $3
		ORDER BY at DESC, id DESC LIMIT $4`, owner, repo, environment, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("query releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []usecase.Release
	for rows.Next() {
		var r usecase.Release
		var prevID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.At, &r.Environment, &r.CommitSHA, &r.ImageRef, &r.Status, &r.Health, &r.Actor, &r.RunURL, &prevID); err != nil {
			return nil, 0, fmt.Errorf("scan release: %w", err)
		}
		if prevID.Valid {
			r.PrevReleaseID = &prevID.Int64
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
