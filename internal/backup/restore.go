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

// restoreIfAbsent restores rel into dst ONLY when dst does not exist. Restore
// never overwrites: an existing file is live data, and replacing it is reserved
// for the explicit `knomit restore` command.
//
// Returns an error satisfying isNoSnapshot (unwrapped, for callers to classify)
// when the replica holds no backup for rel.
func (m *Manager) restoreIfAbsent(ctx context.Context, rel, dst string) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", dst, err)
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
	if st.RemoteTXID > st.LocalTXID {
		return fmt.Errorf("%w: %q local=%d remote=%d (another writer, or a stale volume)",
			ErrDiverged, name, st.LocalTXID, st.RemoteTXID)
	}
	return nil
}
