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
// exists — a replica that decodes into pre-swap or mixed content, and exactly
// the divergence Preflight refuses to start on. resume() therefore re-registers
// the database from scratch AND discards litestream's local LTX state, which is
// how litestream is told "take a fresh snapshot" (DB.ResetLocalState).
//
// Discarding local state does NOT restart the chain at transaction 1: on the
// next open litestream sees the local position behind the replica's, re-anchors
// to the replica's latest transaction, and writes the fresh snapshot as the
// transaction AFTER it. Monotonicity — the property Preflight checks — is
// preserved across the swap.
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
		return noop, fmt.Errorf("backup.Pause %q: %w", name, err)
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
