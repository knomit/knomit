package repos

import (
	"fmt"

	"github.com/rs/zerolog/log"
)

// migrateCredential moves a repo's origin credential out of its own database
// and into control.db, once.
//
// # Why there is no "migrated" flag
//
// A non-empty auth_token in the store IS the unmigrated signal, and migrating
// empties it. That makes the check self-describing and idempotent without a
// bookkeeping column that could itself drift out of step with the thing it
// describes.
//
// # Why this can refuse to serve
//
// A repo whose credential cannot be moved is a repo whose credential is not
// recoverable — it will fail at disaster time instead of now. Refusing makes
// that visible while it is still fixable. The blast radius is deliberately
// narrow: only a repo that ACTUALLY holds a credential can be refused, so
// SSH, public, local and origin-less repos are never affected by a missing
// agent key.
//
// Returning an error means "do not serve this repo". Returning nil means it is
// migrated, or had nothing to migrate.
//
// # It must run BEFORE reconcileOrigin
//
// reconcileOrigin's materialize-down path writes through store.SetRemote, which
// is INSERT OR REPLACE with EMPTY auth columns — so on any boot where control.db
// disagrees about the URL it BLANKS the store's auth_method/auth_token. Run
// after it, this would read an auth column that was just emptied, conclude
// "nothing to migrate", and the only copy of the credential would be gone with
// no error and no failing test. See the ordering note at the call site.
func (m *Manager) migrateCredential(name string, ri *RepoInstance) error {
	reg := m.RepoRegistry()
	if reg == nil || ri == nil {
		return nil
	}
	// Fail CLOSED. Every caller reaches here just after its own open SUCCEEDED,
	// so nothing upstream covers this and "assume there was nothing to migrate"
	// would be a guess about the one thing we must not guess about. Unreachable
	// in practice — only a concurrent Archive/Close of this same name can make
	// Acquire fail this soon after an open — and NOT reachable from a missing
	// agent key, which is read at open time and never surfaces here. The
	// blast-radius promise (a credential-less repo is never refused) therefore
	// still holds.
	svc, release, err := ri.Acquire()
	if err != nil {
		return fmt.Errorf("store could not be acquired to check for a credential: %w", err)
	}
	defer release()

	// LegacyAuth, never GetRemote: GetRemote answers a decrypt failure with the
	// stored bytes "as plaintext", and encrypting THOSE would produce a
	// double-encrypted credential that looks migrated and can never
	// authenticate. This is the one call site where that distinction decides
	// whether a credential survives.
	method, token, err := svc.Remote().LegacyAuth("origin")
	if err != nil {
		return fmt.Errorf("credential cannot be read: %w", err)
	}
	if token == "" {
		// A method without a token is deliberately dropped rather than carried
		// over: there is no secret to move, and nothing downstream needs it.
		// resolveAuthWithOrigin re-derives "ssh" from the URL scheme, and ""
		// and "none" both resolve to the same anonymous access, so the method
		// alone carries no information control.db has to remember.
		return nil // nothing to migrate
	}

	if err := reg.SetOriginCredential(name, method, token); err != nil {
		return fmt.Errorf("credential cannot be recorded in control.db: %w", err)
	}
	// Only now is the store's copy redundant. Ordering matters: interrupted
	// before this, the store still holds the original and the next boot retries
	// from a clean state; the migration never leaves a partial result.
	//
	// A failure here does NOT refuse the repo. control.db already holds the
	// credential, which is the whole point of the migration; the store's copy is
	// merely a duplicate that a later boot will clear.
	if err := svc.Remote().ClearAuth("origin"); err != nil {
		log.Warn().Err(err).Str("repo", name).
			Msg("credential migrated to control.db but the repo's own copy could not be cleared; " +
				"it is redundant and will be cleared on a later boot")
	}
	log.Info().Str("repo", name).Msg("origin credential migrated into control.db")
	return nil
}

// gateCredential is the door EVERY path that puts a repo into service must pass
// it through: it migrates the credential, and takes the repo back OUT of service
// if that cannot be done. nil means the repo may be served.
//
// # Why all three call sites need it, not just boot
//
// OriginAuth reads credentials from control.db with no fallback to the store's
// legacy columns, and its licence to do so is the promise that an unmigrated
// repo is NEVER served (see origin_write.go). A path that serves a repo without
// this gate breaks that promise silently and in the worst possible direction:
// control.db's auth_token is empty, so OriginCredential returns no error, the
// server default applies, resolveAuth yields nil, and the repo syncs
// ANONYMOUSLY against its private origin. Rescan advertises itself as how a repo
// Start skipped comes back, so leaving it ungated would have handed operators a
// one-request bypass of the gate boot had just applied.
//
// So this is called from Start (manager.go), Rescan (manager.go) and Restore
// (lifecycle.go) — the three m.Add sites that put a repo into service holding a
// database this process has not vetted.
//
// # The two m.Add sites that are deliberately NOT gated
//
// There are five m.Add call sites in all, and the next person adding a sixth
// needs to know why these two are exempt rather than guessing from the list:
//
//   - Create (lifecycle.go) — the database was made moments ago by initLocal,
//     which writes no remotes row at all, or initClone, which writes the row with
//     EMPTY auth columns on purpose (Create's registry write-through is what
//     carries the credential into control.db). An empty auth_token IS the migrated
//     marker, so there is provably nothing to migrate.
//   - reinstateLive (lifecycle.go) — Archive's rollback. It re-registers the same
//     repo that was in service a moment earlier, which means it already passed
//     this gate on its way in (or was created above); re-gating would re-check a
//     store nothing has written since.
//
// Both exemptions rest on the store's auth columns, not on convenience. A new
// m.Add whose database came from anywhere this process did not just write —
// disk, an archive, a copy, a backup — needs the gate.
//
// # It must run after the ACTIVE registry row exists
//
// SetOriginCredential updates `WHERE name = ? AND archive_id = ''` and errors
// when that matches nothing, so calling this before the caller has written its
// active row would refuse a perfectly healthy repo. In Start the row is what was
// listed to begin with; Rescan and Restore both call this AFTER their own
// registry write.
//
// Refusal never rolls the caller's work back. The repo keeps its registry row
// and its database exactly as they are, so the state stays diagnosable and the
// next boot retries by itself — the same contract Start's other terminal
// branches offer.
func (m *Manager) gateCredential(name, dbPath string) error {
	inst := m.Get(name)
	err := m.migrateCredential(name, inst)
	if err == nil {
		return nil
	}
	// Both halves matter. unregister alone would leave a repo that is invisible
	// to the API but still holds its SQLite handle, its task hub, and its sync
	// and index-heal goroutines for the life of the process.
	if inst != nil {
		inst.shutdown()
	}
	m.unregister(name)
	log.Error().Err(err).Str("repo", name).
		Msgf("repo %q holds an origin credential that cannot be moved into control.db,"+
			" so it will NOT appear in the API."+
			"\nUntil this is resolved the credential is stored only inside %s,"+
			" and losing that file would make the repo unrecoverable."+
			"\nThe usual cause is an agent key that changed or became unreadable: %s."+
			"\nEither restore that key, or re-authenticate the origin after removing"+
			" the stale credential with:"+
			"\n  sqlite3 %s \"UPDATE remotes SET auth_method='', auth_token='' WHERE name='origin';\"",
			name, dbPath, m.deps.KeyPath, dbPath)
	return err
}
