package store

import (
	"database/sql"
	"fmt"
	"time"
)

// remoteIndex owns remote configuration and git sync/push operations.
type remoteIndex struct {
	rh    *repoHandler
	fi    *factIndex
	si    *searchIndex
	crypt *Crypt
}

var _ RemoteIndex = (*remoteIndex)(nil)

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

// SetRemote inserts or replaces a remote configuration and wires the git
// remote in the underlying repository so that Sync and Push can use it
// immediately. authMethod and authToken are optional; if authToken is
// non-empty it is encrypted at rest when a Crypt instance is configured.
func (ri *remoteIndex) SetRemote(name, url, branch string, interval, pushInterval int, authMethod, authToken string) error {
	storedToken := authToken
	if ri.crypt != nil && authToken != "" {
		enc, err := ri.crypt.encrypt(authToken)
		if err != nil {
			return fmt.Errorf("encrypt token: %w", err)
		}
		storedToken = enc
	}
	_, err := ri.rh.db.Exec(
		`INSERT OR REPLACE INTO remotes (name, url, branch, interval, push_interval, auth_method, auth_token) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, url, branch, interval, pushInterval, authMethod, storedToken,
	)
	if err != nil {
		return err
	}
	// Sync the git config so go-git can fetch/push by remote name.
	// No-op when the repo has not been initialised yet (DB-only mode).
	if ri.rh.repo != nil {
		if err := ri.rh.configureRemote(url, branch); err != nil {
			return fmt.Errorf("configure git remote: %w", err)
		}
	}
	return nil
}

// GetRemote reads a remote configuration by name.
func (ri *remoteIndex) GetRemote(name string) (*Remote, error) {
	r := &Remote{}
	err := ri.rh.db.QueryRow(
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
	if ri.crypt != nil && r.AuthToken != "" {
		dec, decErr := ri.crypt.decrypt(r.AuthToken)
		if decErr != nil {
			// May be plaintext from before encryption was enabled — use as-is.
			_ = decErr
		} else {
			r.AuthToken = dec
		}
	}
	return r, nil
}

// updateRemoteStatus updates the pull-sync status fields for a remote.
func (ri *remoteIndex) updateRemoteStatus(name, status string, syncErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.db.Exec(
		`UPDATE remotes SET last_sync_at = ?, last_status = ?, last_error = ? WHERE name = ?`,
		now, status, syncErr, name,
	)
	return err
}

// updateRemotePushStatus updates the push status fields for a remote.
func (ri *remoteIndex) updateRemotePushStatus(name, status string, pushErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.db.Exec(
		`UPDATE remotes SET last_push_at = ?, last_push_status = ?, last_push_error = ? WHERE name = ?`,
		now, status, pushErr, name,
	)
	return err
}
