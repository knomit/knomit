package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/backupproto"
	"knomit/internal/repos"
)

// ErrDiverged means a local database exists whose transaction history does not
// match the replica's. That indicates two writers, or an old volume reattached.
// It is never auto-recovered: resolving it by resetting the replica would
// discard backup history to hide a correctness problem.
var ErrDiverged = errors.New("local database has diverged from its replica")

// errNoSnapshot means the replica holds no backup at that prefix.
//
// It is unexported because it is not a decision callers make — they read the
// three-way Report instead — but it is a sentinel rather than a string because
// the distinction it draws is load-bearing: "no backup exists" is a normal
// first boot, "the restore errored" is not, and a repo whose restore FAILED
// must never be silently replaced by empty state that replication then writes
// over the good backup.
//
// The classification happens in the AGENT, where litestream's sentinels live,
// and crosses the pipe as backupproto.CodeNoSnapshot. An error string could not
// carry it.
var errNoSnapshot = errors.New("the replica holds no backup for this database")

// isNoSnapshot reports whether err means "the replica holds no backup here".
func isNoSnapshot(err error) bool { return errors.Is(err, errNoSnapshot) }

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

// RestoreArchived pulls an archived repo's database back from the archive
// prefix when dbPath is absent, and reports whether it wrote anything.
//
// This is the half of the archive round trip that makes the other half mean
// something. Bootstrap restores only ACTIVE repos, so after a container
// replacement control.db comes back — and with it every archived registry row,
// so the archive is still advertised as restorable — while
// repos/archive/<id>.db does not. Without this the unarchive would fail on a
// missing file while the never-expiring copy sat in the bucket unused.
//
// It is deliberately lazy rather than part of boot: archives are cold data and
// there can be many of them, so paying to rehydrate every one on every boot to
// serve an unarchive that may never come is the wrong trade. Restore calls this
// at the moment the database is actually wanted.
//
// "No backup exists" is reported as (false, nil), not as an error, so the caller
// can tell "the replica has nothing for this archive" from "the restore broke"
// — the same distinction Report draws for repos, and the two demand different
// messages.
func (m *Manager) RestoreArchived(archiveID, dbPath string) (bool, error) {
	if m == nil {
		return false, nil
	}
	restored, err := m.restoreIfAbsent(context.Background(), m.relFor(ArchiveName(archiveID)), dbPath)
	if isNoSnapshot(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("restore archived %q: %w", archiveID, err)
	}
	if restored {
		log.Info().Str("id", archiveID).Str("db", dbPath).Msg("restored an archived database from the replica")
	}
	return restored, nil
}

// restoreIfAbsent asks the agent to restore rel into dst, and ONLY when dst
// does not exist. Restore never overwrites: an existing file is live data, and
// replacing it is reserved for the explicit `knomit restore` command.
//
// The absence check, the orphaned-sidecar sweep and the directory creation all
// happen agent-side, in one round trip, because they must be atomic with the
// restore itself: a check here and a write there is a race, and the sidecar
// sweep exists precisely to guarantee nothing foreign is beside the file when
// the restore lands.
//
// Returns an error satisfying isNoSnapshot when the replica holds no backup for
// rel.
func (m *Manager) restoreIfAbsent(ctx context.Context, rel, dst string) (bool, error) {
	var res backupproto.RestoreResult
	err := m.cl.call(ctx, backupproto.MethodRestore, backupproto.RestoreParams{Rel: rel, Dest: dst}, &res)
	if err != nil {
		return false, err
	}
	return res.Restored, nil
}

// RestoreTo restores name into dst, OVERWRITING dst if it exists. A zero at
// restores the latest state available; otherwise it is a point in time.
//
// This is the explicit operator path — `knomit restore` — and the ONLY one
// allowed to replace an existing database. The automatic boot restore fills
// absences and nothing else, which is what makes it safe to run unattended; the
// cost of that is that it cannot help a database which is PRESENT and corrupt.
// This is the answer to that case, and it is deliberately a command a human has
// to type.
//
// It must run against a STOPPED server. The agent refuses a destination it is
// itself replicating, but nothing here can see another knomit process holding
// the same file — the refusal covers the mistake this command can make, not
// every mistake possible.
//
// A replica with no backup for name is an error rather than a quiet no-op: the
// operator asked for their data back, and "there is none" is the answer they
// need, not silence.
func (m *Manager) RestoreTo(ctx context.Context, name, dst string, at time.Time) error {
	if m == nil {
		return fmt.Errorf("backup is not enabled")
	}
	// No result decoded: the absent-only path reports "did I write anything",
	// which is a question this one cannot answer with a no — an overwriting
	// restore either wrote the file or returned an error.
	err := m.cl.call(ctx, backupproto.MethodRestore, backupproto.RestoreParams{
		Rel:       m.relFor(name),
		Dest:      dst,
		Overwrite: true,
		Timestamp: at,
	}, nil)
	if isNoSnapshot(err) {
		return fmt.Errorf("the replica holds no backup for %q at %s: %w",
			name, m.relFor(name), err)
	}
	if err != nil {
		return fmt.Errorf("restore %q to %s: %w", name, dst, err)
	}
	log.Info().Str("db", name).Str("dest", dst).Msg("restored a database from the replica, replacing the local file")
	return nil
}

// Preflight verifies that an EXISTING local database still matches its replica.
// A diverged pair means the replica was advanced by another writer, or this
// volume is stale — either way, starting would corrupt the backup.
func (m *Manager) Preflight(ctx context.Context, name, dbPath string) error {
	if m == nil {
		return nil
	}
	return m.cl.call(ctx, backupproto.MethodPreflight, backupproto.PreflightParams{
		Name: name,
		Path: dbPath,
		Rel:  m.relFor(name),
	}, nil)
}
