package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/benbjohnson/litestream"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
)

// ErrDiverged means a local database exists whose transaction history does not
// match the replica's. That indicates two writers, or an old volume reattached.
// It is never auto-recovered: resolving it by resetting the replica would
// discard backup history to hide a correctness problem.
var ErrDiverged = errors.New("local database has diverged from its replica")

// Report is the outcome of restoring a set of repositories.
//
// The three outcomes are deliberately distinct. "No backup exists" is a normal
// first-boot state; "the restore errored" is not. Callers branch on exactly that
// difference: a repo with no snapshot may be rebuilt from origin, while a repo
// whose restore FAILED must not be silently replaced by empty state, because
// replication would then overwrite the good backup.
//
// Restored, NoSnapshot, and Failed do NOT partition the full intended set: a
// repo whose local file already existed is silently omitted from all three —
// restoreIfAbsent never touches it, so it is neither restored, missing a
// snapshot, nor failed. Do not assume every name in intended appears in one
// of these three lists.
type Report struct {
	Restored   []string
	NoSnapshot []string
	Failed     map[string]error
}

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
//     ErrTxNotAvailable }`). This is the path exercised by every test here,
//     since the test replicas are pure LTX (no legacy v0.3.x data).
//   - The legacy v0.3.x restore path (RestoreV3, only reachable when the
//     replica client also implements ReplicaClientV3 and shouldUseV3Restore
//     picks it) returns the exported litestream.ErrNoSnapshots directly.
//
// This isn't a guess: litestream's own db.(*DB).EnsureExists — the built-in
// "restore into an absent local path" helper — classifies "no backup" with
// exactly this pair: `errors.Is(err, ErrTxNotAvailable) || errors.Is(err,
// ErrNoSnapshots)`. We mirror that rather than the published-docs sentinel,
// which alone would misclassify every LTX-path "no backup" as a hard failure.
func isNoSnapshot(err error) bool {
	return errors.Is(err, litestream.ErrTxNotAvailable) || errors.Is(err, litestream.ErrNoSnapshots)
}

// RestoreControl restores control.db when it is absent locally. A missing backup
// is not an error — it is how a first boot looks.
func (m *Manager) RestoreControl(ctx context.Context) error {
	if m == nil {
		return nil
	}
	dst := filepath.Join(m.home, "control.db")
	restored, err := m.restoreIfAbsent(ctx, m.relFor("control"), dst)
	if err != nil {
		if isNoSnapshot(err) {
			return nil
		}
		return fmt.Errorf("restore control.db: %w", err)
	}
	if restored {
		log.Info().Msg("restored control.db from replica")
	}
	return nil
}

// RestoreRepos restores every intended repo whose database file is absent.
func (m *Manager) RestoreRepos(ctx context.Context, intended []repos.RepoRecord) (Report, error) {
	rep := Report{Failed: map[string]error{}}
	if m == nil {
		return rep, nil
	}
	for _, rec := range intended {
		dst := filepath.Join(m.home, "repos", rec.Name+".db")
		restored, err := m.restoreIfAbsent(ctx, m.relFor(rec.Name), dst)
		switch {
		case isNoSnapshot(err):
			rep.NoSnapshot = append(rep.NoSnapshot, rec.Name)
		case err != nil:
			rep.Failed[rec.Name] = err
		case restored:
			rep.Restored = append(rep.Restored, rec.Name)
		}
	}
	log.Info().
		Strs("restored", rep.Restored).
		Strs("no_snapshot", rep.NoSnapshot).
		Int("failed", len(rep.Failed)).
		Msg("repo restore complete")
	return rep, nil
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
// deletion (`rm core.db`), an interrupted wipe, and a crash inside Create's own
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
// outcome this function exists to prevent. The caller routes that into
// Report.Failed, which refuses the boot.
func clearOrphanedSidecars(dbPath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		p := dbPath + suffix
		if _, err := os.Lstat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat %s: %w", p, err)
		}
		log.Warn().Str("path", p).
			Msg("removing orphaned SQLite sidecar with no database beside it; it would be replayed onto the restored file")
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("orphaned %s could not be removed, and restoring around it would let SQLite replay foreign WAL frames into the restored database: %w", p, err)
		}
	}
	return nil
}

// restoreIfAbsent restores rel into dst ONLY when dst does not exist. Restore
// never overwrites: an existing file is live data, and replacing it is reserved
// for the explicit `knomit restore` command.
//
// "Absent" means the .db file specifically — so any -wal/-shm beside it are
// orphans of a previous incarnation and are cleared first; see
// clearOrphanedSidecars.
//
// Returns an error satisfying isNoSnapshot (unwrapped, for callers to classify)
// when the replica holds no backup for rel.
func (m *Manager) restoreIfAbsent(ctx context.Context, rel, dst string) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", dst, err)
	}

	if err := clearOrphanedSidecars(dst); err != nil {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, fmt.Errorf("mkdir for %s: %w", dst, err)
	}

	client, err := litestream.NewReplicaClientFromURL(m.prefix(rel))
	if err != nil {
		return false, fmt.Errorf("replica client for %s: %w", rel, err)
	}
	replica := litestream.NewReplicaWithClient(nil, client)

	opt := litestream.NewRestoreOptions()
	opt.OutputPath = dst
	if err := replica.Restore(ctx, opt); err != nil {
		return false, err
	}
	return true, nil
}

// Preflight verifies that an EXISTING local database still matches its replica.
// A diverged pair means the replica was advanced by another writer, or this
// volume is stale — either way, starting would corrupt the backup.
func (m *Manager) Preflight(ctx context.Context, name, dbPath string) error {
	if m == nil {
		return nil
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil // nothing local to conflict with
	}

	db := litestream.NewDB(dbPath)
	client, err := litestream.NewReplicaClientFromURL(m.prefix(m.relFor(name)))
	if err != nil {
		return fmt.Errorf("preflight %q: replica client: %w", name, err)
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
		return fmt.Errorf("preflight %q: %w", name, err)
	}
	// LocalTXID 0 means the database has NO local litestream state — no LTX
	// directory beside it, nothing that claims a position in the chain. That is
	// not divergence, and refusing it would make the whole backup feature
	// unusable: restoreIfAbsent writes only the .db file, so EVERY boot
	// following a restore looks exactly like this and would refuse to start
	// with "another writer, or a stale volume". It is also the state Pause's
	// ResetLocalState passes through, so a crash mid-swap would poison the next
	// boot the same way.
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
		return fmt.Errorf("%w: %q local=%d remote=%d (another writer, or a stale volume)",
			ErrDiverged, name, st.LocalTXID, st.RemoteTXID)
	}
	return nil
}
