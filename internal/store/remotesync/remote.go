package remotesync

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
)

// Index owns remote configuration and git sync/push operations.
// All git-level plumbing (signer, authorSig, committerSig, NotifyCommit,
// PopulateCommitLog) lives on repoHandler; Index reaches UP via ri.rh.*
// and never through a sibling subsystem.
type Index struct {
	rh *Repo

	// originMu guards origin. Index.SetOrigin re-points a running repo's
	// origin (the HAL PUT/DELETE handlers do this on a live repo) while
	// runReconcileLoop's background goroutine concurrently calls GetRemote —
	// without a lock those are a bare read/write race on the same field.
	originMu sync.RWMutex
	origin   *Origin // injected from control.db; nil = this repo has no origin
	// consensus caches the recorded consensus branch (consensus_branch.go in this package),
	// under originMu.
	consensus string
	// warnAmbiguousOnce: an unrecorded repo with several candidate branches.
	warnAmbiguousOnce sync.Once
}

// NewIndex builds the remote index over rh. The parent's constructor calls it
// once per Service.
func NewIndex(rh *Repo) *Index { return &Index{rh: rh} }

// warnAmbiguousOnceDo runs f the first time it is called on this index and
// never again: the consensus-branch resolution warns once per repo that an
// unrecorded repository has several candidate branches.
func (ri *Index) warnAmbiguousOnceDo(f func()) { ri.warnAmbiguousOnce.Do(f) }

// setConsensus caches the recorded consensus branch (consensus_branch.go in
// this package decides it), under originMu.
func (ri *Index) setConsensus(b string) {
	ri.originMu.Lock()
	ri.consensus = b
	ri.originMu.Unlock()
}

// cachedConsensus returns the cached consensus branch ("" until one is cached).
func (ri *Index) cachedConsensus() string {
	ri.originMu.RLock()
	defer ri.originMu.RUnlock()
	return ri.consensus
}

// SetOrigin supplies the repo's remote connection from control.db. A nil
// origin means this repo has none, which GetRemote reports as (nil, nil) — the
// contract every sync path branches on. Must be called before OpenRepo so
// rehydrateUpstreamMain and the fetch refspec see it.
//
// Safe to call on a running repo: the origin HAL handlers re-point a live
// repo's origin while the background reconcile loop concurrently calls
// GetRemote, so the write goes through originMu rather than a bare field
// assignment.
//
// A non-nil origin with a branch also RECORDS that branch as the repo's
// consensus branch (consensus_branch.go), after the origin is stored, so
// removing the origin later (SetOrigin(nil)) leaves the repo on the branch it
// had, never on a default.
func (ri *Index) SetOrigin(o *Origin) {
	ri.originMu.Lock()
	ri.origin = o
	ri.originMu.Unlock()
	if o != nil && o.Branch != "" {
		ri.RecordConsensusBranch(o.Branch)
	}
}

// getOrigin reads the injected origin under a read lock. See originMu.
func (ri *Index) getOrigin() *Origin {
	ri.originMu.RLock()
	defer ri.originMu.RUnlock()
	return ri.origin
}

// Origin is a repo's remote connection, supplied from OUTSIDE the store. The
// store no longer owns this: <home>/control.db does, so a lost .db can be
// re-cloned from a record that outlives it. AuthToken is plaintext — the
// caller holds the Crypt now, and the store holds no Crypt at all.
//
// Wired via Index.SetOrigin (svc.Remote().SetOrigin), the same ambient-configuration idiom as
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
func (ri *Index) DeleteRemote(name string) error {
	if _, err := ri.rh.vr.DB().Exec(`DELETE FROM remotes WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete remote row: %w", err)
	}
	if ri.rh.vr.Repository() != nil {
		if err := ri.rh.vr.Repository().DeleteRemote(name); err != nil && !errors.Is(err, gogit.ErrRemoteNotFound) {
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
func (ri *Index) GetRemote(name string) (*Remote, error) {
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
	err := ri.rh.vr.DB().QueryRow(
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
func (ri *Index) updateRemoteStatus(name, status string, syncErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.vr.DB().Exec(
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
func (ri *Index) RecordSyncError(name, msg string) error {
	return ri.updateRemoteStatus(name, "error", &msg)
}

// RecordPushError persists a push failure (last_push_status="error",
// last_push_error=msg) on the named remote: the fleet master's refused
// PublishUpstream, which records nothing itself.
func (ri *Index) RecordPushError(name, msg string) error {
	return ri.updateRemotePushStatus(name, "error", &msg)
}

// updateRemotePushStatus updates the push status fields for a remote, creating
// the status row if absent. See updateRemoteStatus.
func (ri *Index) updateRemotePushStatus(name, status string, pushErr *string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ri.rh.vr.DB().Exec(
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
