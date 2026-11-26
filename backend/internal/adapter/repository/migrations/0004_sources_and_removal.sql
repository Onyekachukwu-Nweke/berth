-- 0004: admins decide where developers may deploy from, and can remove people.

-- A removed person is signed out and can't sign in again until restored.
ALTER TABLE users ADD COLUMN revoked_at TIMESTAMPTZ;

-- Where developers may deploy from: an account (user or org) and a repo-name
-- pattern ('*' for all, or e.g. 'meals-*'). Stored lowercased. With no rows,
-- the account that owns the GitHub App is the only source.
CREATE TABLE deploy_sources (
	account    TEXT        NOT NULL,
	pattern    TEXT        NOT NULL DEFAULT '*',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (account, pattern)
);
