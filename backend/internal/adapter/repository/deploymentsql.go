package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO), registers "sqlite"
)

// schemaVersion is bumped whenever the schema below changes shape.
//
// mountabo carries no migrations. It is a local-first tool whose entire database
// is a single file of derived state — which servers and repos you configured,
// and a log of deploys — all of which is recreated by redeploying. Migration
// code for that is pure liability: it can only ever run on a developer's own
// machine, it is exercised by nothing real, and a rebuild that silently drops a
// column is far worse than a clear instruction to start clean.
//
// An older database is therefore refused, by version, with an actionable
// message, rather than being rescued in place.
const schemaVersion = 1

// schema is the whole database, stated once. deployments holds current state
// (one row per target); deploy_events is an append-only log, one row per
// release, carrying the artifact and health fields rollback and release history
// build on.
//
// The deployment identity is (owner, repo, environment): a repo can have several
// first-class environments (production, staging, ...), each with its own branch,
// server, domain and release history. branch is an attribute of the environment,
// not part of the key. deployment_id is the canonical name every server-side
// resource derives from (see usecase.DeriveDeploymentID); it is written once and
// never updated, because it names containers, images and paths that already exist.
const schema = `
CREATE TABLE IF NOT EXISTS deployments (
	owner TEXT NOT NULL,
	repo TEXT NOT NULL,
	branch TEXT NOT NULL,
	id TEXT NOT NULL,
	app TEXT NOT NULL,
	environment TEXT NOT NULL,
	server_id TEXT NOT NULL,
	workflow_file TEXT NOT NULL,
	port INTEGER NOT NULL DEFAULT 0,
	root_dir TEXT NOT NULL DEFAULT '',
	deploy_config TEXT NOT NULL DEFAULT '',
	deployment_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	PRIMARY KEY (owner, repo, environment)
);
CREATE TABLE IF NOT EXISTS deploy_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner TEXT NOT NULL,
	repo TEXT NOT NULL,
	branch TEXT NOT NULL,
	environment TEXT NOT NULL,
	server_id TEXT NOT NULL,
	app TEXT NOT NULL,
	at TEXT NOT NULL,
	commit_sha TEXT NOT NULL DEFAULT '',
	image_ref TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	health TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL DEFAULT '',
	run_url TEXT NOT NULL DEFAULT '',
	prev_event_id INTEGER
);
CREATE INDEX IF NOT EXISTS idx_deploy_events_target ON deploy_events(owner, repo, environment, at);
`

// ErrSchemaTooOld is returned when the database on disk predates the current
// schema. It names the file so the operator can act on it directly.
type ErrSchemaTooOld struct {
	Path string
	Got  int
	Want int
}

func (e *ErrSchemaTooOld) Error() string {
	return fmt.Sprintf("the mountabo database at %s was written by an older version (schema v%d, this build needs v%d).\n"+
		"mountabo keeps only derived state — your servers, deployments and history are recreated by reconnecting and redeploying.\n"+
		"Delete the file and start it again:\n\n    rm %s\n", e.Path, e.Got, e.Want, e.Path)
}

// OpenSQLite opens (creating if needed) the SQLite database at path and applies
// the schema. A database written by an older schema is refused rather than
// migrated: see schemaVersion. The caller owns the returned *sql.DB.
func OpenSQLite(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// user_version is SQLite's own per-database integer, stored in the file
	// header. A brand-new file reads 0, which is indistinguishable from a
	// pre-versioning database — so the tables decide: no tables means new.
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if version == 0 {
		var tables int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('deployments', 'deploy_events')`).Scan(&tables); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("inspect database: %w", err)
		}
		if tables > 0 {
			_ = db.Close()
			return nil, &ErrSchemaTooOld{Path: path, Got: 0, Want: schemaVersion}
		}
	} else if version != schemaVersion {
		_ = db.Close()
		return nil, &ErrSchemaTooOld{Path: path, Got: version, Want: schemaVersion}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// PRAGMA does not accept a bound parameter, and schemaVersion is an untainted
	// integer constant.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("record schema version: %w", err)
	}
	return db, nil
}

// DeploymentSQL persists configured deployments in SQLite and records an
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

// NewDeploymentSQL returns a SQLite-backed deployment store over an open db.
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
		var createdAt, deployConfig string
		if err := rows.Scan(&d.ID, &d.App, &d.Owner, &d.Repo, &d.Branch, &d.Environment, &d.ServerID, &d.WorkflowFile, &d.Port, &d.RootDir, &deployConfig, &d.DeploymentID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		if deployConfig != "" {
			var cfg usecase.DeployConfig
			if json.Unmarshal([]byte(deployConfig), &cfg) == nil {
				d.Config = &cfg
			}
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
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
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deployConfig := ""
	if d.Config != nil {
		if b, merr := json.Marshal(d.Config); merr == nil {
			deployConfig = string(b)
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		usecase.DeploymentIDFor(d), createdAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("upsert deployment: %w", err)
	}

	// Link this release to the environment's previous one (NULL for the first), so
	// release history is a chain rollback can walk. Computed inside the same
	// transaction so a concurrent deploy cannot slot a release in between.
	var prevID sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(id) FROM deploy_events WHERE owner = ? AND repo = ? AND environment = ?`,
		d.Owner, d.Repo, d.Environment).Scan(&prevID); err != nil {
		return fmt.Errorf("find previous release: %w", err)
	}

	// A release is 'pending' until its GitHub Actions run completes; the terminal
	// status and run_url are overlaid live from the run at read time. image_ref
	// and health are provisioned but populated by later phases.
	if _, err := tx.Exec(`INSERT INTO deploy_events
		(owner, repo, branch, environment, server_id, app, at, commit_sha, actor, status, image_ref, health, run_url, prev_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', '', '', '', ?)`,
		d.Owner, d.Repo, d.Branch, d.Environment, d.ServerID, d.App, now, d.CommitSHA, d.Actor, prevID); err != nil {
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
	res, err := s.db.Exec(`UPDATE deployments SET port = ? WHERE owner = ? AND repo = ? AND environment = ?`,
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
	if _, err := tx.Exec(`DELETE FROM deploy_events WHERE owner = ? AND repo = ? AND environment = ?`,
		owner, repo, environment); err != nil {
		return false, fmt.Errorf("delete deploy events: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM deployments WHERE owner = ? AND repo = ? AND environment = ?`,
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deploy_events WHERE owner = ? AND repo = ? AND environment = ?`,
		owner, repo, environment).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count releases: %w", err)
	}

	rows, err := s.db.Query(`SELECT id, at, environment, commit_sha, image_ref, status, health, actor, run_url, prev_event_id
		FROM deploy_events
		WHERE owner = ? AND repo = ? AND environment = ?
		ORDER BY at DESC, id DESC LIMIT ?`, owner, repo, environment, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("query releases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []usecase.Release
	for rows.Next() {
		var r usecase.Release
		var at string
		var prevID sql.NullInt64
		if err := rows.Scan(&r.ID, &at, &r.Environment, &r.CommitSHA, &r.ImageRef, &r.Status, &r.Health, &r.Actor, &r.RunURL, &prevID); err != nil {
			return nil, 0, fmt.Errorf("scan release: %w", err)
		}
		r.At, _ = time.Parse(time.RFC3339, at)
		if prevID.Valid {
			r.PrevReleaseID = &prevID.Int64
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
