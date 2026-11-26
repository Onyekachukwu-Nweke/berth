-- Production approval was dropped: admins decide where each environment
-- deploys, and developers with push access deploy.
ALTER TABLE env_rules DROP COLUMN require_approval;
ALTER TABLE repo_rules DROP COLUMN require_approval;
