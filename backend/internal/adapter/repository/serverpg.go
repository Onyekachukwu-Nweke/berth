package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// ServerSQL persists servers in Postgres, one JSONB document per server. It
// satisfies usecase.ServerStore.
type ServerSQL struct {
	db *sql.DB
}

var _ usecase.ServerStore = (*ServerSQL)(nil)

// NewServerSQL returns a Postgres-backed server store over an open db.
func NewServerSQL(db *sql.DB) *ServerSQL {
	return &ServerSQL{db: db}
}

// List returns all servers, oldest first.
func (s *ServerSQL) List() ([]usecase.Server, error) {
	rows, err := s.db.Query(`SELECT data FROM servers ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query servers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []usecase.Server{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		var srv usecase.Server
		if err := json.Unmarshal(raw, &srv); err != nil {
			return nil, fmt.Errorf("decode server: %w", err)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// Get returns one server by id, or usecase.ErrServerNotFound.
func (s *ServerSQL) Get(id string) (usecase.Server, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT data FROM servers WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return usecase.Server{}, usecase.ErrServerNotFound
	}
	if err != nil {
		return usecase.Server{}, fmt.Errorf("get server: %w", err)
	}
	var srv usecase.Server
	if err := json.Unmarshal(raw, &srv); err != nil {
		return usecase.Server{}, fmt.Errorf("decode server: %w", err)
	}
	return srv, nil
}

// Save upserts a server by id.
func (s *ServerSQL) Save(srv usecase.Server) error {
	raw, err := json.Marshal(srv)
	if err != nil {
		return fmt.Errorf("encode server: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO servers (id, data) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET data = excluded.data`, srv.ID, raw); err != nil {
		return fmt.Errorf("save server: %w", err)
	}
	return nil
}

// Delete removes a server by id. Deleting a missing server is not an error.
func (s *ServerSQL) Delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM servers WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	return nil
}
