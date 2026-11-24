package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

// AccessSQL persists who can do what: users and their sessions, each user's
// repo access as GitHub last reported it, and the placement policy (clients,
// repo links, environment rules).
type AccessSQL struct {
	db *sql.DB
}

var (
	_ usecase.UserStore        = (*AccessSQL)(nil)
	_ usecase.SessionStore     = (*AccessSQL)(nil)
	_ usecase.RepoAccessReader = (*AccessSQL)(nil)
	_ usecase.RepoAccessWriter = (*AccessSQL)(nil)
	_ usecase.PolicyStore      = (*AccessSQL)(nil)
)

// NewAccessSQL returns the access store over an open db.
func NewAccessSQL(db *sql.DB) *AccessSQL {
	return &AccessSQL{db: db}
}

// ErrUserNotFound is returned when no user has the given id.
var ErrUserNotFound = errors.New("user not found")

// --- users -----------------------------------------------------------------

const userColumns = `id, github_id, login, name, avatar_url, is_admin, repos_synced_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (usecase.User, error) {
	var u usecase.User
	var synced sql.NullTime
	if err := row.Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &u.IsAdmin, &synced, &u.LastLoginAt); err != nil {
		return usecase.User{}, err
	}
	if synced.Valid {
		u.ReposSyncedAt = synced.Time
	}
	return u, nil
}

// UpsertUser creates or refreshes a user by GitHub id (logins can change; the
// id cannot) and returns the stored row.
func (s *AccessSQL) UpsertUser(u usecase.User) (usecase.User, error) {
	row := s.db.QueryRow(`INSERT INTO users (github_id, login, name, avatar_url, is_admin, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (github_id) DO UPDATE SET
			login = excluded.login, name = excluded.name, avatar_url = excluded.avatar_url,
			is_admin = excluded.is_admin, last_login_at = excluded.last_login_at
		RETURNING `+userColumns,
		u.GitHubID, u.Login, u.Name, u.AvatarURL, u.IsAdmin, u.LastLoginAt)
	out, err := scanUser(row)
	if err != nil {
		return usecase.User{}, fmt.Errorf("upsert user: %w", err)
	}
	return out, nil
}

// GetUser returns a user by id, or ErrUserNotFound.
func (s *AccessSQL) GetUser(id int64) (usecase.User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return usecase.User{}, ErrUserNotFound
	}
	if err != nil {
		return usecase.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// ListUsers returns everyone who has signed in, most recent first.
func (s *AccessSQL) ListUsers() ([]usecase.User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY last_login_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []usecase.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- sessions --------------------------------------------------------------

// CreateSession stores a session by its token hash. Expired sessions are swept
// on the way in, so the table never grows without bound.
func (s *AccessSQL) CreateSession(hash []byte, userID int64, expires time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return fmt.Errorf("sweep sessions: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`, hash, userID, expires); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionUser returns the user id behind a live session, or an error when the
// session is unknown or expired.
func (s *AccessSQL) SessionUser(hash []byte, now time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT user_id FROM sessions WHERE token_hash = $1 AND expires_at > $2`, hash, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("session: %w", err)
	}
	return id, nil
}

// DeleteSession ends a session. It is idempotent.
func (s *AccessSQL) DeleteSession(hash []byte) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = $1`, hash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// --- repo access -----------------------------------------------------------

// ReplaceRepoAccess swaps a user's cached repo access for GitHub's latest
// answer and stamps when it was read, in one transaction.
func (s *AccessSQL) ReplaceRepoAccess(userID int64, repos []usecase.RepoAccess, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM user_repos WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear repo access: %w", err)
	}
	for _, r := range repos {
		if r.Permission < usecase.PermRead {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO user_repos (user_id, owner, repo, permission) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, owner, repo) DO UPDATE SET permission = excluded.permission`,
			userID, r.Owner, r.Repo, r.Permission.String()); err != nil {
			return fmt.Errorf("save repo access: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE users SET repos_synced_at = $2 WHERE id = $1`, userID, at); err != nil {
		return fmt.Errorf("stamp repo sync: %w", err)
	}
	return tx.Commit()
}

// RepoAccessFor returns the repos a user can reach, as last synced.
func (s *AccessSQL) RepoAccessFor(userID int64) ([]usecase.RepoAccess, error) {
	rows, err := s.db.Query(`SELECT owner, repo, permission FROM user_repos WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("query repo access: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []usecase.RepoAccess{}
	for rows.Next() {
		var r usecase.RepoAccess
		var perm string
		if err := rows.Scan(&r.Owner, &r.Repo, &perm); err != nil {
			return nil, fmt.Errorf("scan repo access: %w", err)
		}
		r.Permission = usecase.ParsePermission(perm)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- placement policy ------------------------------------------------------

// Policy reads the whole placement policy in one snapshot.
func (s *AccessSQL) Policy() (usecase.Policy, error) {
	var p usecase.Policy

	rows, err := s.db.Query(`SELECT id, name, created_at FROM clients ORDER BY name`)
	if err != nil {
		return p, fmt.Errorf("query clients: %w", err)
	}
	for rows.Next() {
		var c usecase.Client
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("scan client: %w", err)
		}
		p.Clients = append(p.Clients, c)
	}
	_ = rows.Close()

	rows, err = s.db.Query(`SELECT owner, repo, client_id FROM repo_links ORDER BY owner, repo`)
	if err != nil {
		return p, fmt.Errorf("query links: %w", err)
	}
	for rows.Next() {
		var l usecase.RepoLink
		if err := rows.Scan(&l.Owner, &l.Repo, &l.ClientID); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("scan link: %w", err)
		}
		p.Links = append(p.Links, l)
	}
	_ = rows.Close()

	rows, err = s.db.Query(`SELECT client_id, environment, server_id FROM env_rules ORDER BY client_id, environment`)
	if err != nil {
		return p, fmt.Errorf("query env rules: %w", err)
	}
	for rows.Next() {
		var r usecase.EnvRule
		if err := rows.Scan(&r.ClientID, &r.Environment, &r.ServerID); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("scan env rule: %w", err)
		}
		p.EnvRules = append(p.EnvRules, r)
	}
	_ = rows.Close()

	rows, err = s.db.Query(`SELECT owner, repo, environment, server_id FROM repo_rules ORDER BY owner, repo, environment`)
	if err != nil {
		return p, fmt.Errorf("query repo rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r usecase.RepoRule
		if err := rows.Scan(&r.Owner, &r.Repo, &r.Environment, &r.ServerID); err != nil {
			return p, fmt.Errorf("scan repo rule: %w", err)
		}
		p.RepoRules = append(p.RepoRules, r)
	}
	return p, rows.Err()
}

// SaveClient creates or renames a client.
func (s *AccessSQL) SaveClient(c usecase.Client) error {
	if _, err := s.db.Exec(`INSERT INTO clients (id, name) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name`, c.ID, c.Name); err != nil {
		return fmt.Errorf("save client: %w", err)
	}
	return nil
}

// DeleteClient removes a client; its links and rules go with it (cascade).
func (s *AccessSQL) DeleteClient(id string) error {
	if _, err := s.db.Exec(`DELETE FROM clients WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	return nil
}

// SaveLink assigns a repo (or "*") to a client. Owner and repo are stored
// lowercased: GitHub names are case-insensitive.
func (s *AccessSQL) SaveLink(l usecase.RepoLink) error {
	if _, err := s.db.Exec(`INSERT INTO repo_links (owner, repo, client_id) VALUES ($1, $2, $3)
		ON CONFLICT (owner, repo) DO UPDATE SET client_id = excluded.client_id`,
		strings.ToLower(l.Owner), strings.ToLower(l.Repo), l.ClientID); err != nil {
		return fmt.Errorf("save link: %w", err)
	}
	return nil
}

// DeleteLink removes a repo link.
func (s *AccessSQL) DeleteLink(owner, repo string) error {
	if _, err := s.db.Exec(`DELETE FROM repo_links WHERE owner = $1 AND repo = $2`, strings.ToLower(owner), strings.ToLower(repo)); err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	return nil
}

// SaveEnvRule sets where a client's environment deploys.
func (s *AccessSQL) SaveEnvRule(r usecase.EnvRule) error {
	if _, err := s.db.Exec(`INSERT INTO env_rules (client_id, environment, server_id) VALUES ($1, $2, $3)
		ON CONFLICT (client_id, environment) DO UPDATE SET server_id = excluded.server_id`,
		r.ClientID, strings.ToLower(r.Environment), r.ServerID); err != nil {
		return fmt.Errorf("save env rule: %w", err)
	}
	return nil
}

// DeleteEnvRule removes a client's environment rule.
func (s *AccessSQL) DeleteEnvRule(clientID, environment string) error {
	if _, err := s.db.Exec(`DELETE FROM env_rules WHERE client_id = $1 AND environment = $2`, clientID, strings.ToLower(environment)); err != nil {
		return fmt.Errorf("delete env rule: %w", err)
	}
	return nil
}

// SaveRepoRule sets a repo's environment override.
func (s *AccessSQL) SaveRepoRule(r usecase.RepoRule) error {
	if _, err := s.db.Exec(`INSERT INTO repo_rules (owner, repo, environment, server_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner, repo, environment) DO UPDATE SET server_id = excluded.server_id`,
		strings.ToLower(r.Owner), strings.ToLower(r.Repo), strings.ToLower(r.Environment), r.ServerID); err != nil {
		return fmt.Errorf("save repo rule: %w", err)
	}
	return nil
}

// DeleteRepoRule removes a repo override.
func (s *AccessSQL) DeleteRepoRule(owner, repo, environment string) error {
	if _, err := s.db.Exec(`DELETE FROM repo_rules WHERE owner = $1 AND repo = $2 AND environment = $3`,
		strings.ToLower(owner), strings.ToLower(repo), strings.ToLower(environment)); err != nil {
		return fmt.Errorf("delete repo rule: %w", err)
	}
	return nil
}
