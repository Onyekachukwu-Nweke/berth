-- 0002: who can do what. Identity comes from GitHub (sign-in through the
-- GitHub App); repo access is GitHub's answer, cached per user; where code may
-- run is berth's own policy: clients, the servers they own, and per-client
-- environment rules.

CREATE TABLE users (
	id              BIGSERIAL   PRIMARY KEY,
	github_id       BIGINT      NOT NULL UNIQUE,
	login           TEXT        NOT NULL,
	name            TEXT        NOT NULL DEFAULT '',
	avatar_url      TEXT        NOT NULL DEFAULT '',
	is_admin        BOOLEAN     NOT NULL DEFAULT false,
	repos_synced_at TIMESTAMPTZ,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_login_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only a hash of the session token is stored; the token itself lives in the
-- user's cookie.
CREATE TABLE sessions (
	token_hash BYTEA       PRIMARY KEY,
	user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions (user_id);

-- The repos a user can reach through the App, and their permission on each,
-- as GitHub last reported it.
CREATE TABLE user_repos (
	user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	owner      TEXT   NOT NULL,
	repo       TEXT   NOT NULL,
	permission TEXT   NOT NULL,
	PRIMARY KEY (user_id, owner, repo)
);

CREATE TABLE clients (
	id         TEXT        PRIMARY KEY,
	name       TEXT        NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The agency itself: repos not linked to any client resolve here.
INSERT INTO clients (id, name) VALUES ('agency', 'Agency (internal)');

-- Which client a repo belongs to. repo = '*' links every repo of an owner
-- (e.g. a client's own GitHub org); an exact repo row wins over it.
CREATE TABLE repo_links (
	owner     TEXT NOT NULL,
	repo      TEXT NOT NULL,
	client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
	PRIMARY KEY (owner, repo)
);

-- Where a client's environments deploy: e.g. acme/development -> the agency's
-- shared dev box, acme/production -> acme's own server.
CREATE TABLE env_rules (
	client_id        TEXT    NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
	environment      TEXT    NOT NULL,
	server_id        TEXT    NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
	require_approval BOOLEAN NOT NULL DEFAULT false,
	PRIMARY KEY (client_id, environment)
);

-- A single repo's exception to its client's rule for one environment.
CREATE TABLE repo_rules (
	owner            TEXT    NOT NULL,
	repo             TEXT    NOT NULL,
	environment      TEXT    NOT NULL,
	server_id        TEXT    NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
	require_approval BOOLEAN NOT NULL DEFAULT false,
	PRIMARY KEY (owner, repo, environment)
);
