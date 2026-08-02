package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

	if err := a.restoreInto(ctx, p.Rel, p.Dest); err != nil {
		return false, err
	}
	return true, nil
}

// restoreInto downloads rel into out, which must not exist — litestream's
// Restore refuses a destination that does. It always restores the latest state:
// the replica is a warm-start cache, and point-in-time selection belonged to the
// operator restore command that no longer exists.
func (a *Agent) restoreInto(ctx context.Context, rel, out string) error {
	client, err := litestream.NewReplicaClientFromURL(a.prefixFor(rel))
	if err != nil {
		return fmt.Errorf("replica client for %s: %w", rel, err)
	}
	replica := litestream.NewReplicaWithClient(nil, client)

	opt := litestream.NewRestoreOptions()
	opt.OutputPath = out
	if err := replica.Restore(ctx, opt); err != nil {
		if isNoSnapshot(err) {
			return withCode(proto.CodeNoSnapshot, err)
		}
		return err
	}
	return nil
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
// outcome this function exists to prevent. The client reports that repo as not
// restored, and it is rebuilt from its origin instead.
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
