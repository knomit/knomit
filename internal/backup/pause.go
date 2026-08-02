package backup

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"

	"knomit/internal/backup/proto"
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
// state (the agent's reset_local_state, litestream's DB.ResetLocalState).
//
// Note what is NO LONGER the reason for pausing. While litestream ran inside
// knomit's process, an untrack was also needed to stop two SQLite builds from
// fighting over the same file, because their locks did not conflict. The agent
// is a separate process now, so the kernel arbitrates and that hazard is gone.
// What remains is the chain-identity problem above, which no amount of locking
// would ever have solved.
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
// AFTER it, so the chain stays monotonic across the swap. The re-anchor is
// asynchronous — it runs on the first monitor tick, not inside Track — so
// between the reset and it there is a window where local LTX state is absent,
// which is also exactly how a freshly restored database looks.
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

	m.mu.RLock()
	entry, tracked := m.dbs[name]
	closed := m.closed
	m.mu.RUnlock()
	// A closed manager has already stopped every replica: there is nothing to
	// pause, and resuming would only fail with "manager is closed" — a
	// distracting error to attach to a swap that happens during shutdown.
	if !tracked || closed {
		return noop, nil
	}
	dbPath := entry.path

	// Untrack closes the agent's litestream database and flushes a final sync
	// of the PRE-swap state, so the chain is complete up to the moment the file
	// is replaced.
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
	// Recorded only now, on the path where the database really is untracked, so
	// Status keeps reporting it instead of letting it disappear. The failure
	// path above re-tracked it, and marking that as paused would invent a state
	// nothing is going to clear.
	m.mu.Lock()
	m.paused[name] = struct{}{}
	m.mu.Unlock()
	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: paused replication")

	var once sync.Once
	var resumeErr error
	return func() error {
		// Once, because resume is wired as a deferred call: a caller that also
		// invokes it explicitly must not get a second reset_local_state, which
		// would wipe the local LTX state of the now-LIVE database behind its
		// back.
		once.Do(func() { resumeErr = m.resume(name, dbPath) })
		return resumeErr
	}, nil
}

// resume re-registers a paused database, forcing a fresh snapshot.
//
// The reset is addressed by PATH rather than by name: between the pause and
// here the name is not tracked, and litestream's local state lives entirely in
// paths derived from the database file.
func (m *Manager) resume(name, dbPath string) error {
	if err := m.cl.call(context.Background(), proto.MethodResetLocalState,
		proto.ResetLocalStateParams{Path: dbPath}, nil); err != nil {
		return fmt.Errorf("backup.Pause: resume %q: reset local state: %w", name, err)
	}
	if err := m.Track(name, dbPath); err != nil {
		return fmt.Errorf("backup.Pause: resume %q: %w", name, err)
	}
	// Cleared only after Track has succeeded. A resume that failed leaves the
	// name marked paused, which is the honest report — it is not being
	// replicated — and it keeps the database on the status surface instead of
	// dropping it out of both maps.
	m.mu.Lock()
	delete(m.paused, name)
	m.mu.Unlock()
	log.Info().Str("db", name).Str("path", dbPath).Msg("backup: resumed replication with a fresh snapshot")
	return nil
}
