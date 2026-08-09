package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
)

// remoteIndex owns remote configuration and git sync/push operations.
// All git-level plumbing (signer, authorSig, committerSig, notifyCommit,
// populateCommitLog) lives on repoHandler; remoteIndex reaches UP via ri.rh.*
// and never through a sibling subsystem.
type remoteIndex struct {
	rh *repoHandler

	// originMu guards origin. Service.SetOrigin re-points a running repo's
	// origin (the HAL PUT/DELETE handlers do this on a live repo) while
	// runReconcileLoop's background goroutine concurrently calls GetRemote —
	// without a lock those are a bare read/write race on the same field.
	originMu sync.RWMutex
	origin   *Origin // injected from control.db; nil = this repo has no origin
}

// setOrigin stores the injected origin under a write lock. See originMu.
func (ri *remoteIndex) setOrigin(o *Origin) {
	ri.originMu.Lock()
	ri.origin = o
	ri.originMu.Unlock()
}

// getOrigin reads the injected origin under a read lock. See originMu.
func (ri *remoteIndex) getOrigin() *Origin {
	ri.originMu.RLock()
	defer ri.originMu.RUnlock()
	return ri.origin
}

var _ RemoteIndex = (*remoteIndex)(nil)

// Origin is a repo's remote connection, supplied from OUTSIDE the store. The
// store no longer owns this: <home>/control.db does, so a lost .db can be
// re-cloned from a record that outlives it. AuthToken is plaintext — the
// caller holds the Crypt now, and the store holds no Crypt at all.
//
// Wired via Service.SetOrigin, the same ambient-configuration idiom as
// SetSigner / SetOntologyRoot.
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

// DeleteRemote tears down this repo's use of a remote: it removes the git
// remote so neither sync nor push can reach it, and drops the remotes row.
//
// The row no longer holds connection identity (control.db does) — it is pure
// sync/push STATUS. It is deleted anyway, because status describes a
// relationship that no longer exists: a repo later re-pointed at a DIFFERENT
// origin would otherwise inherit the previous one's last_sync_at/last_error and
// report them as its own. A missing row and a missing git remote are both
// tolerated, so the call is idempotent.
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
// There is no longer a fallback to the repo's own columns — migration 000017
// dropped them, so an uninjected origin is the WHOLE answer, not a hint to look
// elsewhere.
func (ri *remoteIndex) GetRemote(name string) (*Remote, error) {
	origin := ri.getOrigin()
	if origin == nil {
		return nil, nil
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

// defaultSyncInterval is the poll cadence used when no status row exists yet,
// and the one the status upserts seed a fresh row with.
const defaultSyncInterval = 300

// updateRemoteStatus updates the pull-sync status fields for a remote,
// creating the status row if this is the first sync. The row is no longer
// guaranteed to exist: connection config lives in control.db now, so nothing
// inserts it ahead of time.
func (ri *remoteIndex) updateRemoteStatus(name, status string, syncErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.db.Exec(
		`INSERT INTO remotes (name, interval, push_interval, last_sync_at, last_status, last_error)
		 VALUES (?, ?, ?, ?, ?, ?)
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
		`INSERT INTO remotes (name, interval, push_interval, last_push_at, last_push_status, last_push_error)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		     last_push_at     = excluded.last_push_at,
		     last_push_status = excluded.last_push_status,
		     last_push_error  = excluded.last_push_error`,
		name, defaultSyncInterval, defaultSyncInterval, now, status, pushErr,
	)
	return err
}
