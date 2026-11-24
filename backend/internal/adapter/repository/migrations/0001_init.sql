-- 0001: the hosted foundation. Servers, deployments and their release log, and
-- the encrypted vault that replaces the OS keychain.

CREATE TABLE servers (
	id         TEXT PRIMARY KEY,
	-- The full usecase.Server record. Servers are read and written whole, so a
	-- document column keeps the store lossless as the struct grows.
	data       JSONB       NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
	owner         TEXT        NOT NULL,
	repo          TEXT        NOT NULL,
	environment   TEXT        NOT NULL,
	id            TEXT        NOT NULL,
	branch        TEXT        NOT NULL,
	app           TEXT        NOT NULL,
	server_id     TEXT        NOT NULL,
	workflow_file TEXT        NOT NULL,
	port          INTEGER     NOT NULL DEFAULT 0,
	root_dir      TEXT        NOT NULL DEFAULT '',
	deploy_config JSONB,
	-- Written once, never updated: it names containers, images and paths that
	-- already exist on the server (see usecase.DeriveDeploymentID).
	deployment_id TEXT        NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (owner, repo, environment)
);

CREATE TABLE deploy_events (
	id            BIGSERIAL   PRIMARY KEY,
	owner         TEXT        NOT NULL,
	repo          TEXT        NOT NULL,
	environment   TEXT        NOT NULL,
	branch        TEXT        NOT NULL,
	server_id     TEXT        NOT NULL,
	app           TEXT        NOT NULL,
	at            TIMESTAMPTZ NOT NULL DEFAULT now(),
	commit_sha    TEXT        NOT NULL DEFAULT '',
	image_ref     TEXT        NOT NULL DEFAULT '',
	status        TEXT        NOT NULL DEFAULT 'pending',
	health        TEXT        NOT NULL DEFAULT '',
	actor         TEXT        NOT NULL DEFAULT '',
	run_url       TEXT        NOT NULL DEFAULT '',
	prev_event_id BIGINT      REFERENCES deploy_events(id) ON DELETE SET NULL
);
CREATE INDEX idx_deploy_events_target ON deploy_events (owner, repo, environment, at DESC);

-- The platform's own credentials (server SSH keys, deploy keys, tool admin
-- logins). Sealed with AES-256-GCM under MOUNTABO_MASTER_KEY; the database
-- never sees a plaintext value. App secrets are NOT here: those live only in
-- GitHub Actions secrets.
CREATE TABLE vault_secrets (
	key        TEXT        PRIMARY KEY,
	sealed     BYTEA       NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
