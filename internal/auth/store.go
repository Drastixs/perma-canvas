package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// User is the Canvas user the tokens belong to.
type User struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Tokens is the persisted OAuth grant obtained through the Canvas mobile flow.
type Tokens struct {
	Domain          string    `json:"canvas_domain"`
	Purpose         string    `json:"purpose"`
	ClientID        string    `json:"client_id"`
	ClientSecret    string    `json:"client_secret"`
	AccessToken     string    `json:"access_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	RefreshToken    string    `json:"refresh_token"`
	User            User      `json:"user"`
	ObtainedAt      time.Time `json:"obtained_at"`
	RefreshedAt     time.Time `json:"last_refreshed_at"`
}

// Store persists Tokens as a 0600 JSON file.
type Store struct{ path string }

func NewStore(dir string) *Store { return &Store{path: filepath.Join(dir, "tokens.json")} }

func (s *Store) Path() string { return s.path }

// Load returns (nil, nil) when no tokens have been stored yet.
func (s *Store) Load() (*Tokens, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return &t, nil
}

func (s *Store) Save(t *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
