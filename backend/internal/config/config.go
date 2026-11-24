// Package config loads mountabo's runtime configuration from the environment
// (12-factor style) and returns a concrete *Config. Secrets such as the GitHub
// client secret are read here and passed inward; they are never logged.
package config

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config is mountabo's resolved runtime configuration.
type Config struct {
	// HTTPAddr is the address the API listens on. It defaults to loopback; a
	// hosted install sets it (e.g. ":7778") and puts the API behind its proxy.
	HTTPAddr        string
	ShutdownTimeout time.Duration
	GitHub          GitHubConfig
	AI              AIConfig
	// DatabaseURL is the Postgres connection string (DATABASE_URL).
	DatabaseURL string
	// MasterKey seals the platform's own credentials (server SSH keys, deploy
	// keys) in the database. 32 bytes, from base64 MOUNTABO_MASTER_KEY. Losing
	// it makes every sealed credential unreadable; rotate it deliberately.
	MasterKey []byte
}

// Validate reports the first required setting that is missing or malformed, so
// the server refuses to start rather than failing on its first request.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required (e.g. postgres://mountabo:pass@localhost:5432/mountabo)")
	}
	if len(c.MasterKey) != 32 {
		return errors.New("MOUNTABO_MASTER_KEY must be 32 random bytes, base64-encoded; generate one with: openssl rand -base64 32")
	}
	return nil
}

// AIConfig holds the Anthropic credentials for the AI command helper. APIKey is
// optional: when empty, the helper reports "not configured" rather than failing,
// so mountabo runs fine without it. Model overrides the default Claude model.
type AIConfig struct {
	APIKey string
	Model  string
}

// GitHubConfig holds the OAuth App credentials used to exchange authorization
// codes for access tokens.
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
}

// Load reads configuration from the environment, applying defaults. Call
// Validate before using it.
// It first loads the repository-root .env (shared with the frontend) if present.
func Load() *Config {
	loadDotenv()
	return &Config{
		HTTPAddr:        env("MOUNTABO_HTTP_ADDR", "127.0.0.1:7778"),
		ShutdownTimeout: 10 * time.Second,
		GitHub: GitHubConfig{
			ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
			ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		},
		AI: AIConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  os.Getenv("MOUNTABO_AI_MODEL"),
		},
		DatabaseURL: os.Getenv("DATABASE_URL"),
		MasterKey:   decodeKey(os.Getenv("MOUNTABO_MASTER_KEY")),
	}
}

// decodeKey decodes a base64 master key, returning nil when it is unset or not
// valid base64 (Validate then reports it).
func decodeKey(v string) []byte {
	if v == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil
	}
	return b
}

// loadDotenv loads the repository-root .env if one exists, searching upward from
// the working directory so it is found whether the binary runs from the repo
// root or from backend/. Real environment variables always win, godotenv.Load
// never overrides values already set. A shipped binary with no .env simply uses
// the OS environment.
func loadDotenv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, statErr := os.Stat(path); statErr == nil {
			if loadErr := godotenv.Load(path); loadErr != nil {
				slog.Warn("loading .env", "path", path, "err", loadErr)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached the filesystem root without finding a .env
		}
		dir = parent
	}
}

// env returns the value of key, or fallback when key is unset or empty.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
