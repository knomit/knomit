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
	rh     *repoHandler
	crypt  *Crypt
	origin *Origin // injected from control.db; nil = this repo has no origin
}

var _ RemoteIndex = (*remoteIndex)(nil)

// Origin is a repo's remote connection, supplied from OUTSIDE the store. The
// store no longer owns this: <home>/control.db does, so a lost .db can be
// re-cloned from a record that outlives it. AuthToken is plaintext — the
// caller holds the Crypt now.
//
// Wired via Service.SetOrigin, the same ambient-configuration idiom as
// SetCrypt / SetSigner / SetOntologyRoot.
type Origin struct {
	URL        string
	Branch     string
	AuthMethod string
	AuthToken  string
}

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
		enc, err := ri.crypt.Encrypt(authToken)
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
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("SetUpstreamBranch: no remote %q configured", name)
	}
	if err != nil {
		return fmt.Errorf("SetUpstreamBranch: read remote %q: %w", name, err)
	}
	if url == "" {
		// A status-only row (url='') left by updateRemoteStatus/
		// updateRemotePushStatus is not a configured connection — see
		// legacyRemoteRow, which draws the same line one function over.
		// Without this check we'd fall through to configureRemote("") and
		// wire up a git remote with an empty URL instead of reporting that
		// there is none.
		return fmt.Errorf("SetUpstreamBranch: no remote %q configured", name)
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
//
// Connection identity (url, branch, auth) comes from the INJECTED origin —
// control.db owns it. Sync/push STATUS comes from this repo's own remotes row:
// it describes the local replica and is meaningless after a rehydrate. The two
// are assembled into the single *Remote callers have always received.
//
// A nil injected origin means the repo has no origin, reported as (nil, nil)
// regardless of any status row left behind by a previously-configured one.
//
// TRANSITIONAL: while the remotes table still carries url/branch/auth columns,
// an absent injected origin falls back to reading them, so writers can migrate
// one at a time. Task 18 drops the columns and this fallback with them.
func (ri *remoteIndex) GetRemote(name string) (*Remote, error) {
	origin := ri.origin
	if origin == nil {
		legacy, err := ri.legacyRemoteRow(name)
		if err != nil || legacy == nil {
			return nil, err
		}
		origin = legacy
	}

	r := &Remote{
		Name:       name,
		URL:        origin.URL,
		Branch:     origin.Branch,
		AuthMethod: origin.AuthMethod,
		AuthToken:  origin.AuthToken,
	}
	// Status is optional: a repo whose origin was just configured has no row
	// until its first sync, and that is not an error.
	err := ri.rh.db.QueryRow(
		`SELECT interval, last_sync_at, last_status, last_error,
		        push_interval, last_push_at, last_push_status, last_push_error
		   FROM remotes WHERE name = ?`, name,
	).Scan(&r.Interval, &r.LastSyncAt, &r.LastStatus, &r.LastError,
		&r.PushInterval, &r.LastPushAt, &r.LastPushStatus, &r.LastPushError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		r.Interval, r.PushInterval = defaultSyncInterval, defaultSyncInterval
	}
	return r, nil
}

// defaultSyncInterval is the poll cadence used when no status row exists yet.
// Matches the value every SetRemote call site passes today.
const defaultSyncInterval = 300

// legacyRemoteRow reads connection identity from the pre-migration remotes
// columns. TRANSITIONAL — deleted in the task that drops those columns.
func (ri *remoteIndex) legacyRemoteRow(name string) (*Origin, error) {
	var o Origin
	err := ri.rh.db.QueryRow(
		`SELECT url, branch, auth_method, auth_token FROM remotes WHERE name = ?`, name,
	).Scan(&o.URL, &o.Branch, &o.AuthMethod, &o.AuthToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if o.URL == "" {
		return nil, nil // status-only row left by a migrated writer
	}
	if ri.crypt != nil && o.AuthToken != "" {
		if dec, decErr := ri.crypt.Decrypt(o.AuthToken); decErr != nil {
			log.Warn().Err(decErr).Str("remote", name).
				Msg("remote: token decrypt failed; using stored value as plaintext")
		} else {
			o.AuthToken = dec
		}
	}
	return &o, nil
}

// updateRemoteStatus updates the pull-sync status fields for a remote,
// creating the status row if this is the first sync. The row is no longer
// guaranteed to exist: connection config lives in control.db now, so nothing
// inserts it ahead of time.
func (ri *remoteIndex) updateRemoteStatus(name, status string, syncErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.db.Exec(
		`INSERT INTO remotes (name, url, branch, interval, push_interval, last_sync_at, last_status, last_error)
		 VALUES (?, '', '', ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     last_sync_at = excluded.last_sync_at,
		     last_status  = excluded.last_status,
		     last_error   = excluded.last_error`,
		name, defaultSyncInterval, defaultSyncInterval, now, status, syncErr,
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

// updateRemotePushStatus updates the push status fields for a remote, creating
// the status row if absent. See updateRemoteStatus.
func (ri *remoteIndex) updateRemotePushStatus(name, status string, pushErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.db.Exec(
		`INSERT INTO remotes (name, url, branch, interval, push_interval, last_push_at, last_push_status, last_push_error)
		 VALUES (?, '', '', ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     last_push_at     = excluded.last_push_at,
		     last_push_status = excluded.last_push_status,
		     last_push_error  = excluded.last_push_error`,
		name, defaultSyncInterval, defaultSyncInterval, now, status, pushErr,
	)
	return err
}
