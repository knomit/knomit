package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/rs/zerolog/log"
)

// remoteIndex owns remote configuration and git sync/push operations.
// All git-level plumbing (signer, authorSig, committerSig, notifyCommit,
// populateCommitLog) lives on repoHandler; remoteIndex reaches UP via ri.rh.*
// and never through a sibling subsystem.
type remoteIndex struct {
	rh    *repoHandler
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
// immediately.
//
// upstreamMain is the remote's consensus branch (typically "main" but
// configurable to "master" or any other name). It is stored in
// Remote.Branch and woven into the fetch refspec. Empty defaults to "main"
// — callers that have already discovered the right name (e.g. via the
// connectivity-test UI flow) should pass it explicitly.
//
// agentBranch is the LOCAL agent branch this machine writes to
// (e.g. "agent/<host>"); it is woven into the fetch refspec so
// origin/agent/<host> is tracked alongside origin/<upstreamMain>.
//
// authMethod and authToken are optional; if authToken is non-empty it is
// encrypted at rest when a Crypt instance is configured.
func (ri *remoteIndex) SetRemote(name, url, upstreamMain, agentBranch string, interval, pushInterval int, authMethod, authToken string) error {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	storedToken := authToken
	if authToken != "" {
		// Credentials are NEVER stored in plaintext. If encryption is not
		// configured (the agent key was unreadable when the store was opened —
		// see openStore), refuse the write rather than persist a secret in the
		// clear. Callers surface this so the user can fix the key, then retry.
		if ri.crypt == nil {
			return fmt.Errorf("refusing to store credential for remote %q: encryption unavailable (agent key unreadable); credentials are never stored in plaintext", name)
		}
		enc, err := ri.crypt.encrypt(authToken)
		if err != nil {
			return fmt.Errorf("encrypt token: %w", err)
		}
		storedToken = enc
	}
	_, err := ri.rh.db.Exec(
		`INSERT OR REPLACE INTO remotes (name, url, branch, interval, push_interval, auth_method, auth_token) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, url, upstreamMain, interval, pushInterval, authMethod, storedToken,
	)
	if err != nil {
		return err
	}
	// Sync the git config so go-git can fetch/push by remote name.
	// No-op when the repo has not been initialised yet (DB-only mode).
	if ri.rh.repo != nil {
		if err := ri.rh.configureRemote(url, upstreamMain, agentBranch); err != nil {
			return fmt.Errorf("configure git remote: %w", err)
		}
	}
	return nil
}

// SetUpstreamBranch changes the configured consensus ("main") branch for an
// existing remote WITHOUT touching its stored auth. It updates Remote.Branch
// and rewrites the git fetch refspec (via configureRemote) so the next Sync
// fetches and reconciles against the new upstream.
//
// Use this to recover from a degenerate config where upstreamMain was set to
// the agent branch (which makes reconcileNow go push-only — see its guard):
// point it back at a real consensus branch such as "main". agentBranch is this
// machine's local agent branch, preserved in the refspec.
func (ri *remoteIndex) SetUpstreamBranch(name, upstreamMain, agentBranch string) error {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	var url string
	err := ri.rh.db.QueryRow(`SELECT url FROM remotes WHERE name = ?`, name).Scan(&url)
	if err == sql.ErrNoRows {
		return fmt.Errorf("SetUpstreamBranch: no remote %q configured", name)
	}
	if err != nil {
		return fmt.Errorf("SetUpstreamBranch: read remote %q: %w", name, err)
	}
	// Rewrite the git fetch refspec FIRST. The whole point of this call is to
	// make the next Sync reconcile against the new upstream, which only works
	// if the refspec is updated. If the repo isn't initialised we can't do
	// that, so fail WITHOUT touching the stored branch — a DB-only update would
	// leave Remote.Branch and the git refspec permanently inconsistent.
	if ri.rh.repo == nil {
		return fmt.Errorf("SetUpstreamBranch: repository not initialised; cannot rewrite fetch refspec for %q", name)
	}
	if err := ri.rh.configureRemote(url, upstreamMain, agentBranch); err != nil {
		return fmt.Errorf("SetUpstreamBranch: configure git remote: %w", err)
	}
	if _, err := ri.rh.db.Exec(`UPDATE remotes SET branch = ? WHERE name = ?`, upstreamMain, name); err != nil {
		return fmt.Errorf("SetUpstreamBranch: update branch: %w", err)
	}
	return nil
}

// DeleteRemote removes a remote configuration: it deletes the remotes row and
// removes the git remote so neither sync nor push can use it. A missing row and
// a missing git remote are tolerated, so the call is idempotent.
func (ri *remoteIndex) DeleteRemote(name string) error {
	if _, err := ri.rh.db.Exec(`DELETE FROM remotes WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete remote row: %w", err)
	}
	if ri.rh.repo != nil {
		if err := ri.rh.repo.DeleteRemote(name); err != nil && !errors.Is(err, gogit.ErrRemoteNotFound) {
			return fmt.Errorf("delete git remote: %w", err)
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
			// May be plaintext from before encryption was enabled — fall
			// through and use as-is. We can't distinguish "legacy plaintext"
			// from "ciphertext we can no longer decrypt" (rotated key,
			// corruption) without a schema flag, so log at Warn so a real
			// failure is observable instead of surfacing as a confusing 401
			// from the remote when the wrong bytes are presented as auth.
			log.Warn().
				Err(decErr).
				Str("remote", r.Name).
				Msg("remote: token decrypt failed; using stored value as plaintext")
		} else {
			r.AuthToken = dec
		}
	}
	return r, nil
}

// LegacyAuth reads a remote's stored auth WITHOUT GetRemote's lenient
// plaintext fallback.
//
// GetRemote, on a decrypt failure, logs and hands back the STORED BYTES as
// though they were plaintext — sensible for a sync attempt that will simply
// get a 401, and catastrophic for a migration, which would take that
// ciphertext for a token and encrypt it a second time. The result looks
// migrated, decrypts to ciphertext, and can never authenticate again.
//
// So this reports the difference GetRemote deliberately blurs:
//   - no token stored      -> ("", "", nil)
//   - token, decrypted     -> (method, plaintext, nil)
//   - token, undecryptable -> error (rotated key, corruption, or no Crypt)
func (ri *remoteIndex) LegacyAuth(name string) (string, string, error) {
	var method, stored string
	err := ri.rh.db.QueryRow(
		`SELECT auth_method, auth_token FROM remotes WHERE name = ?`, name,
	).Scan(&method, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("legacy auth %q: %w", name, err)
	}
	if stored == "" {
		return method, "", nil
	}
	if ri.crypt == nil {
		return "", "", fmt.Errorf(
			"legacy auth %q: a credential is stored but no encryption key is available", name)
	}
	token, decErr := ri.crypt.Decrypt(stored)
	if decErr != nil {
		return "", "", fmt.Errorf("legacy auth %q: decrypt stored credential: %w", name, decErr)
	}
	return method, token, nil
}

// ClearAuth blanks a remote's auth columns, leaving every other field intact.
// Used by the one-time migration that moves credentials into control.db: an
// empty auth_token is what marks a remote as migrated.
func (ri *remoteIndex) ClearAuth(name string) error {
	if _, err := ri.rh.db.Exec(
		`UPDATE remotes SET auth_method = '', auth_token = '' WHERE name = ?`, name,
	); err != nil {
		return fmt.Errorf("clear auth %q: %w", name, err)
	}
	return nil
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

// RecordSyncError persists a sync failure on the named remote (last_status
// = "error", last_error = msg) WITHOUT running a fetch. The reconcile loop
// uses it to surface an auth-resolution failure — a credential that could not
// be resolved never reaches Sync(), so its error would otherwise not be
// recorded on the remote record. Mirrors the error path Sync() itself takes
// via updateRemoteStatus, so the visible status is identical to a fetch error.
func (ri *remoteIndex) RecordSyncError(name, msg string) error {
	return ri.updateRemoteStatus(name, "error", &msg)
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
