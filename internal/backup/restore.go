package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"knomit/internal/backup/proto"
	"knomit/internal/repos"
)

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
// and crosses the pipe as proto.CodeNoSnapshot. An error string could not
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
//
// Rows whose name is not a legal repo name are SKIPPED rather than restored.
// The name is both a path component and a replica key here, and a row can only
// have got into control.db by hand-editing or from a knomit older than the
// reserved-name guard — but the damage a bad one does is specific and severe: a
// row named "control" makes relFor return the CONTROL DATABASE'S replica path
// while dst stays repos/control.db, so the restore writes the registry's bytes
// into a repo file, and the boot that follows fails on the name collision. This
// is the same defence in depth Manager.Start applies to the same rows, at the
// only other place that turns one into a path.
func (m *Manager) RestoreRepos(ctx context.Context, intended []repos.RepoRecord) (Report, error) {
	rep := Report{Failed: map[string]error{}}
	if m == nil {
		return rep, nil
	}
	for _, rec := range intended {
		if !repos.IsValidName(rec.Name) {
			log.Error().Str("repo", rec.Name).
				Msg("registry row has an invalid or reserved repo name; not restoring it")
			continue
		}
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
// does not exist. Restore never overwrites, and there is no path in knomit that
// does: an existing file is live data, and the replica is a warm-start cache
// whose entire job is to fill an ABSENCE on a cold boot. Overwriting live data
// with it would invert that — trading a database knomit is serving for a copy
// that is at best a monitor interval behind it.
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
	var res proto.RestoreResult
	err := m.cl.call(ctx, proto.MethodRestore, proto.RestoreParams{Rel: rel, Dest: dst}, &res)
	if err != nil {
		return false, err
	}
	return res.Restored, nil
}
