package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Remote represents a configured git remote for sync and push.
type Remote struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	Branch         string  `json:"branch"`
	Interval       int     `json:"interval"`
	LastSyncAt     *string `json:"last_sync_at"`
	LastStatus     *string `json:"last_status"`
	LastError      *string `json:"last_error"`
	PushInterval   int     `json:"push_interval"`
	LastPushAt     *string `json:"last_push_at"`
	LastPushStatus *string `json:"last_push_status"`
	LastPushError  *string `json:"last_push_error"`
	AuthMethod     string  `json:"auth_method,omitempty"`
	AuthToken      string  `json:"auth_token,omitempty"`
}

// SetRemote inserts or replaces a remote configuration.
func (s *Service) SetRemote(name, url, branch string, interval, pushInterval int) error {
	_, err := s.rh.db.Exec(
		`INSERT OR REPLACE INTO remotes (name, url, branch, interval, push_interval) VALUES (?, ?, ?, ?, ?)`,
		name, url, branch, interval, pushInterval,
	)
	return err
}

// SetRemoteWithAuth inserts or replaces a remote configuration including auth credentials.
// Credentials are encrypted at rest if a Crypt instance is configured.
func (s *Service) SetRemoteWithAuth(name, url, branch string, interval, pushInterval int, authMethod, authToken string) error {
	storedToken := authToken
	if s.crypt != nil && authToken != "" {
		enc, err := s.crypt.Encrypt(authToken)
		if err != nil {
			return fmt.Errorf("encrypt token: %w", err)
		}
		storedToken = enc
	}
	_, err := s.rh.db.Exec(
		`INSERT OR REPLACE INTO remotes (name, url, branch, interval, push_interval, auth_method, auth_token) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, url, branch, interval, pushInterval, authMethod, storedToken,
	)
	return err
}

// GetRemote reads a remote configuration by name.
func (s *Service) GetRemote(name string) (*Remote, error) {
	r := &Remote{}
	err := s.rh.db.QueryRow(
		`SELECT name, url, branch, interval, last_sync_at, last_status, last_error,
		        push_interval, last_push_at, last_push_status, last_push_error,
		        auth_method, auth_token
		 FROM remotes WHERE name = ?`,
		name,
	).Scan(&r.Name, &r.URL, &r.Branch, &r.Interval, &r.LastSyncAt, &r.LastStatus, &r.LastError,
		&r.PushInterval, &r.LastPushAt, &r.LastPushStatus, &r.LastPushError,
		&r.AuthMethod, &r.AuthToken)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Decrypt token if encrypted.
	if s.crypt != nil && r.AuthToken != "" {
		dec, decErr := s.crypt.Decrypt(r.AuthToken)
		if decErr != nil {
			// May be plaintext from before encryption was enabled — use as-is.
			_ = decErr
		} else {
			r.AuthToken = dec
		}
	}
	return r, nil
}

// UpdateRemoteStatus updates the pull-sync status fields for a remote.
func (s *Service) UpdateRemoteStatus(name, status string, syncErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.rh.db.Exec(
		`UPDATE remotes SET last_sync_at = ?, last_status = ?, last_error = ? WHERE name = ?`,
		now, status, syncErr, name,
	)
	return err
}

// UpdateRemotePushStatus updates the push status fields for a remote.
func (s *Service) UpdateRemotePushStatus(name, status string, pushErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.rh.db.Exec(
		`UPDATE remotes SET last_push_at = ?, last_push_status = ?, last_push_error = ? WHERE name = ?`,
		now, status, pushErr, name,
	)
	return err
}
