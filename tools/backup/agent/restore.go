package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benbjohnson/litestream"

	"knomit/internal/backup/proto"
)

// isNoSnapshot reports whether err means "the replica holds no backup here" —
// a normal first-boot condition, not a failure.
//
// litestream v0.5.15 signals this two different ways depending on which
// internal path is taken, and neither is the single sentinel the published
// docs suggest:
//
//   - The primary (LTX-format) restore path goes through CalcRestorePlan,
//     which returns ErrTxNotAvailable — wrapped via %w — when it finds zero
//     LTX files to restore from (replica.go: `if len(infos) == 0 { return nil,
//     ErrTxNotAvailable }`). This is the path every test exercises, since the
//     test replicas are pure LTX (no legacy v0.3.x data).
//   - The legacy v0.3.x restore path (RestoreV3, only reachable when the
//     replica client also implements ReplicaClientV3 and shouldUseV3Restore
//     picks it) returns the exported litestream.ErrNoSnapshots directly.
//
// This isn't a guess: litestream's own db.(*DB).EnsureExists — the built-in
// "restore into an absent local path" helper — classifies "no backup" with
// exactly this pair. We mirror that rather than the published-docs sentinel,
// which alone would misclassify every LTX-path "no backup" as a hard failure.
//
// The classification happens HERE, agent-side, and crosses the pipe as
// proto.CodeNoSnapshot: an error string cannot carry error identity, and
// the client's callers branch on precisely this distinction — a repo with no
// snapshot may be rebuilt from origin, while a repo whose restore FAILED must
// not be silently replaced by empty state.
func isNoSnapshot(err error) bool {
	return errors.Is(err, litestream.ErrTxNotAvailable) || errors.Is(err, litestream.ErrNoSnapshots)
}

// Restore restores rel into dst ONLY when dst does not exist. Restore never
// overwrites: an existing file is live data.
//
// "Absent" means the .db file specifically — so any -wal/-shm beside it are
// orphans of a previous incarnation and are cleared first; see
// clearOrphanedSidecars.
func (a *Agent) Restore(ctx context.Context, p proto.RestoreParams) (bool, error) {
	if err := a.requireOpen(); err != nil {
		return false, err
	}
	if p.Rel == "" || p.Dest == "" {
		return false, withCode(proto.CodeBadRequest, fmt.Errorf("restore: rel and dest are required"))
	}
	if p.Overwrite {
		return a.restoreOverwriting(ctx, p)
	}

	if _, err := os.Stat(p.Dest); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", p.Dest, err)
	}

	if err := a.clearOrphanedSidecars(p.Dest); err != nil {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(p.Dest), 0o755); err != nil {
		return false, fmt.Errorf("mkdir for %s: %w", p.Dest, err)
	}

	if err := a.restoreInto(ctx, p.Rel, p.Dest, p.Timestamp); err != nil {
		return false, err
	}
	return true, nil
}

// restoreInto downloads rel into out, which must not exist — litestream's
// Restore refuses a destination that does. A zero at restores the latest state.
func (a *Agent) restoreInto(ctx context.Context, rel, out string, at time.Time) error {
	client, err := litestream.NewReplicaClientFromURL(a.prefixFor(rel))
	if err != nil {
		return fmt.Errorf("replica client for %s: %w", rel, err)
	}
	replica := litestream.NewReplicaWithClient(nil, client)

	opt := litestream.NewRestoreOptions()
	opt.OutputPath = out
	if !at.IsZero() {
		opt.Timestamp = at
	}
	if err := replica.Restore(ctx, opt); err != nil {
		if isNoSnapshot(err) {
			return withCode(proto.CodeNoSnapshot, err)
		}
		return err
	}
	return nil
}

// restoreOverwriting replaces an existing database with the replica's copy. It
// is the explicit operator path, and the only one allowed to destroy live data.
//
// The order below is the whole design, and each step earns its place:
//
//  1. REFUSE if this agent is replicating the destination.
//
//     Be clear about what this does and does not buy. On the shipped path it
//     NEVER fires: `knomit restore` calls backup.Open, which spawns a fresh
//     agent whose tracked set is empty, so nothing is ever registered against
//     the destination. It guards a future in-process caller, and it is cheap
//     insurance against one being added without noticing. Nothing stops an
//     operator restoring under a running server — that is on them.
//
//     The re-check inside the critical section below is the one that matters
//     here: a bare check at the top would be TOCTOU against a concurrent track
//     landing during the download, which can legitimately run for minutes.
//
//  2. Restore into a SIBLING temp path first, never onto the destination. The
//     documented use is recovering a database that is present but corrupt, and
//     a mistyped --timestamp or an object-store failure must not be able to
//     leave the operator with neither the old copy nor a new one. Everything up
//     to step 3 is therefore non-destructive: a failure there leaves the
//     original exactly as it was.
//
//  3. Remove the destination's WAL/SHM/journal BEFORE the rename, not after. A
//     sidecar surviving the rename is replayed onto the restored file by SQLite
//     on first open — a WAL header carries no database identity, so the
//     corruption is silent.
//
//     This is where the operation stops being reversible, and the trade is
//     deliberate rather than free: a rename that fails after this leaves the
//     original .db without its -wal, losing any committed transactions not yet
//     checkpointed into it. The alternative ordering trades that narrow window
//     (two local filesystem operations apart) for silent corruption of the
//     restored database, which is worse and much harder to notice.
//
//  4. Rename, which is atomic within the directory.
//
//  5. Discard litestream's local LTX state for the destination. It describes
//     the file that was just replaced; continuing that chain would upload
//     deltas computed against pages that no longer exist, and it would do so
//     without an error anywhere — the same hazard Pause's reset exists for. On
//     the next open litestream re-anchors against the replica instead.
func (a *Agent) restoreOverwriting(ctx context.Context, p proto.RestoreParams) (bool, error) {
	if a.isTrackedPath(p.Dest) {
		return false, trackedDestErr(p.Dest)
	}
	if err := os.MkdirAll(filepath.Dir(p.Dest), 0o755); err != nil {
		return false, fmt.Errorf("mkdir for %s: %w", p.Dest, err)
	}

	tmp := p.Dest + ".knomit-restore"
	// litestream restores through its own OutputPath+".tmp", so both are
	// cleared: a leftover from an interrupted attempt would make Restore refuse
	// the destination it is about to write.
	for _, leftover := range []string{tmp, tmp + ".tmp"} {
		if err := os.Remove(leftover); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("clear %s from a previous restore: %w", leftover, err)
		}
	}
	defer func() { _ = os.Remove(tmp) }()

	// The download runs OUTSIDE opMu. It can legitimately take minutes, and
	// holding the mutation lock across it would freeze track and untrack for
	// every database for the duration.
	if err := a.restoreInto(ctx, p.Rel, tmp, p.Timestamp); err != nil {
		return false, err
	}

	// opMu for the destructive part only: it is what track and untrack
	// serialise on, so nothing can register the destination between the re-check
	// and the rename. None of the calls below take opMu themselves.
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if a.isTrackedPath(p.Dest) {
		return false, trackedDestErr(p.Dest)
	}

	if err := a.clearSidecars(p.Dest); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, p.Dest); err != nil {
		return false, fmt.Errorf("move the restored database into place at %s: %w", p.Dest, err)
	}
	if err := litestream.NewDB(p.Dest).ResetLocalState(ctx); err != nil {
		return false, fmt.Errorf("restored %s, but litestream's local state for the replaced file could not be "+
			"discarded and would be continued against the new one: %w", p.Dest, err)
	}
	a.logger.Info("restored a database over the existing file", "dest", p.Dest, "rel", p.Rel)
	return true, nil
}

func trackedDestErr(dest string) error {
	return fmt.Errorf("refusing to restore over %s: this agent is replicating it right now; stop the server first", dest)
}

// clearOrphanedSidecars removes SQLite companion files left beside a database
// file that no longer exists, and is called on every path restore is about to
// write.
//
// Why this is not paranoia: restore keys off the .db file alone and writes only
// the .db file, so an orphaned -wal outlives it. SQLite then REPLAYS that WAL
// onto the freshly restored database on first open — a WAL header carries no
// database identity, so SQLite has no way to recognise the frames as foreign,
// and the corruption is silent. The realistic producers are a partial manual
// deletion (`rm core.db`), an interrupted wipe, and a crash inside a
// db → -wal → -shm removal sequence, which is not atomic.
//
// -journal is covered too, for the same hazard in its rollback form: a hot
// journal is replayed on open exactly as a WAL is, and carries no more identity
// than one does. knomit opens every database in WAL mode so a journal should
// never appear, but "should never" is not a reason to leave the one-element gap
// in a function whose entire purpose is to make the restored file's history
// start with the restore.
//
// Deleting is the right disposal, not merely the convenient one: these files
// are deltas over the pages of a specific database file. With that file gone
// there is nothing to apply them to and nothing in them is recoverable, so no
// data is lost by removing them — whereas keeping them can only ever corrupt.
//
// A sidecar we cannot remove is a hard error rather than something to restore
// around: restoring into a path that still holds foreign frames is exactly the
// outcome this function exists to prevent. The client routes that into
// Report.Failed, which refuses the boot.
func (a *Agent) clearOrphanedSidecars(dbPath string) error {
	return a.clearSidecarsBecause(dbPath,
		"removing orphaned SQLite sidecar with no database beside it; it would be replayed onto the restored file")
}

// clearSidecars removes the companion files of a database that is ABOUT TO BE
// REPLACED. Same hazard as clearOrphanedSidecars, reached from the other
// direction: after the overwrite the sidecars describe the old file, and SQLite
// would replay them onto the new one with no way to recognise them as foreign.
func (a *Agent) clearSidecars(dbPath string) error {
	return a.clearSidecarsBecause(dbPath,
		"removing the SQLite sidecars of a database being replaced; they would be replayed onto the restored file")
}

// clearSidecarsBecause does the removal both callers need, logging reason for
// each file it takes away.
func (a *Agent) clearSidecarsBecause(dbPath, reason string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		p := dbPath + suffix
		if _, err := os.Lstat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", p, err)
		}
		a.logger.Warn(reason, "path", p)
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("%s could not be removed, and restoring around it would let SQLite replay foreign WAL frames into the restored database: %w", p, err)
		}
	}
	return nil
}

// Preflight verifies that an EXISTING local database still matches its replica.
// A diverged pair means the replica was advanced by another writer, or this
// volume is stale — either way, starting would corrupt the backup.
func (a *Agent) Preflight(ctx context.Context, p proto.PreflightParams) error {
	if err := a.requireOpen(); err != nil {
		return err
	}
	if p.Path == "" || p.Rel == "" {
		return withCode(proto.CodeBadRequest, fmt.Errorf("preflight: path and rel are required"))
	}
	if _, err := os.Stat(p.Path); os.IsNotExist(err) {
		return nil // nothing local to conflict with
	}

	db := litestream.NewDB(p.Path)
	client, err := litestream.NewReplicaClientFromURL(a.prefixFor(p.Rel))
	if err != nil {
		return fmt.Errorf("preflight %q: replica client: %w", p.Name, err)
	}
	db.Replica = litestream.NewReplicaWithClient(db, client)

	// SyncStatus's remote half (Replica.calcPos) does NOT return isNoSnapshot
	// for an empty replica — MaxLTXFileInfo just returns a zero-value FileInfo
	// with a nil error when its iterator is empty, so RemoteTXID comes back 0
	// with err == nil. The isNoSnapshot check below is defensive only, in case
	// a future litestream version starts surfacing it here; the actual "local
	// file, no replica yet" case is handled by the RemoteTXID(0) > LocalTXID
	// comparison below being false for any non-negative LocalTXID.
	st, err := db.SyncStatus(ctx)
	if err != nil {
		if isNoSnapshot(err) {
			return nil
		}
		return fmt.Errorf("preflight %q: %w", p.Name, err)
	}
	// LocalTXID 0 means the database has NO local litestream state — no LTX
	// directory beside it, nothing that claims a position in the chain. That is
	// not divergence, and refusing it would make the whole backup feature
	// unusable: restore writes only the .db file, so EVERY boot following a
	// restore looks exactly like this and would refuse to start with "another
	// writer, or a stale volume". It is also the state ResetLocalState passes
	// through, so a crash mid-swap would poison the next boot the same way.
	//
	// Litestream self-heals this case on open: with no local position it
	// re-anchors to the replica's latest transaction (checkDatabaseBehindReplica)
	// and continues from there, so no history is lost or overwritten.
	//
	// The cost, stated plainly: a genuinely stale volume that ALSO lost its
	// litestream shadow directory is waved through, and its older content
	// becomes the replica's new head (the replica's earlier history survives
	// underneath — the snapshot lands after it — but the head is wrong). That
	// state is byte-for-byte identical to a fresh restore, so no check here can
	// separate them. Divergence with local state INTACT — the ordinary two-
	// writers and reattached-old-volume cases — still fires below.
	if st.LocalTXID == 0 {
		return nil
	}
	if st.RemoteTXID > st.LocalTXID {
		return withCode(proto.CodeDiverged, fmt.Errorf(
			"local database has diverged from its replica: %q local=%d remote=%d (another writer, or a stale volume)",
			p.Name, st.LocalTXID, st.RemoteTXID))
	}
	return nil
}
