package backup

import (
	"context"
	"fmt"
	"sync"

	"github.com/benbjohnson/litestream"
	"github.com/rs/zerolog/log"
)

// Pause temporarily stops replicating a database and returns a resume function.
//
// This exists for repos.Manager.SwapStore, which replaces a repo's database
// FILE wholesale. A swapped file is a NEW IDENTITY: its transaction IDs no
// longer relate to the ones already in the LTX chain, so continuing that chain
// across the swap uploads deltas computed against a database that no longer
// exists — a replica that decodes into pre-swap or mixed content, and worse, it
// does so SILENTLY: the file is rewritten in place, so litestream sees no frames
// it recognises, uploads nothing, and keeps reporting InSync. resume() therefore
// re-registers the database from scratch AND discards litestream's local LTX
// state (DB.ResetLocalState).
//
// What the reset buys, precisely: on the next open litestream compares the file
// against the newest LOCAL LTX file to decide whether it can continue
// incrementally or must snapshot. Without the reset that file is a leftover from
// the pre-swap chain — a stale description of a database that no longer exists,
// and the decision rests on heuristics (WAL salts, last-page match) applied to
// it. With the reset there is no local file at all, so litestream re-anchors by
// DOWNLOADING the replica's latest transaction and compares against that: the
// mismatch is then decided against the replica's own record rather than a local
// leftover, and the fresh snapshot follows.
//
// Discarding local state does NOT restart the chain at transaction 1: that
// re-anchor (litestream's checkDatabaseBehindReplica) sets the local position to
// the replica's latest, and the fresh snapshot is written as the transaction
// AFTER it. Monotonicity — the property Preflight checks — is preserved across
// the swap. Note the re-anchor is asynchronous (it runs on the first monitor
// tick, not inside Track), so between the reset and it there is a window where
// local LTX state is absent; Preflight treats that state as benign precisely
// because it is also how a freshly restored database looks.
//
// Pause differs from Untrack. Untrack is PERMANENT (archive, purge): the
// database is gone and nothing will replicate it again. Pause is TEMPORARY and
// must always be paired with resume — a paused database that is never resumed
// is a repo that silently stops being backed up.
//
// Pausing an untracked database (or a nil Manager — backup disabled) is a no-op
// returning a no-op resume, so callers need no special-casing.
//
// Pause takes no context: it is synchronous and must have completed before the
// caller touches the file.
func (m *Manager) Pause(name string) (func() error, error) {
	noop := func() error { return nil }
	if m == nil {
		return noop, nil
	}

	// Read the tracked set under the read lock and RELEASE it before calling
	// Untrack, which takes the same (non-reentrant) mutex exclusively.
	m.mu.RLock()
	db, tracked := m.dbs[name]
	closed := m.closed
	m.mu.RUnlock()
	// A closed manager has already stopped every replica: there is nothing to
	// pause, and resuming would only fail with "manager is closed" — a
	// distracting error to attach to a swap that happens during shutdown.
	if !tracked || closed {
		return noop, nil
	}
	dbPath := db.Path()

	// Untrack closes the litestream database, which flushes a final sync of the
	// PRE-swap state and, crucially, drops litestream's SQLite connection before
	// the caller closes its own. knomit's handle then closes last and, without
	// PERSIST_WAL, checkpoints and deletes the -wal — so the file being copied
	// over has no stale sidecar WAL to be replayed onto the new content.
	if err := m.Untrack(name); err != nil {
		// Untrack drops the database from the tracked set before the close that
		// failed, so replication is now OFF and no resume is coming: the caller
		// aborts its swap on this error, and a returned resume it never calls
		// would leave the repo unreplicated until the process restarts. Since
		// the abort means the file is never touched, re-registering here is
		// both safe and complete — the database's identity did not change, so
		// no reset is wanted; the existing chain simply continues.
		if terr := m.Track(name, dbPath); terr != nil {
			return noop, fmt.Errorf("backup.Pause %q: %w (replication NOT restored: %v)", name, err, terr)
		}
		log.Warn().Err(err).Str("db", name).Msg("backup: pause failed; replication left running")
		return noop, fmt.Errorf("backup.Pause %q: %w (replication left running)", name, err)
	}
	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: paused replication")

	var once sync.Once
	var resumeErr error
	return func() error {
		// Once, because resume is wired as a deferred call: a caller that also
		// invokes it explicitly must not get a second ResetLocalState, which
		// would wipe the local LTX state of the now-LIVE database behind its
		// back (its cached position lives in a different DB value).
		once.Do(func() { resumeErr = m.resume(name, dbPath, db) })
		return resumeErr
	}, nil
}

// resume re-registers a paused database, forcing a fresh snapshot.
//
// paused is the closed litestream.DB from before the pause; it is used only for
// its derived local-state paths, which depend on dbPath alone.
func (m *Manager) resume(name, dbPath string, paused *litestream.DB) error {
	if err := paused.ResetLocalState(context.Background()); err != nil {
		return fmt.Errorf("backup.Pause: resume %q: reset local state: %w", name, err)
	}
	if err := m.Track(name, dbPath); err != nil {
		return fmt.Errorf("backup.Pause: resume %q: %w", name, err)
	}
	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: resumed replication with a fresh snapshot")
	return nil
}
