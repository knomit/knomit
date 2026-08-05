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
// Empty store auth columns ARE the migrated signal, and migrating empties them.
// That makes the check self-describing and idempotent without a bookkeeping
// column that could itself drift out of step with the thing it describes. Every
// production writer of those columns (SetOrigin, initClone, reconcileOrigin's
// materialize-down) writes them EMPTY, so a non-empty one can only be a legacy
// row this function has not reached yet.
//
// # The METHOD is carried too, not just the token
//
// A row naming auth_method with no token holds no secret, but it is not
// nothing: OriginAuth reads control.db ONLY, and resolveAuthWithOrigin
// re-infers a method from the URL for git@/ssh:// and for nothing else. Drop
// the method of a legacy http:// origin configured as ssh, and a credential
// that used to fail loudly ("ssh auth requires a key path") resolves to
// ANONYMOUS and reports a green "ok" — the silent downgrade
// test/storytests/contract_auth_resolution_test.go exists to forbid. So the
// early return below means "nothing to carry AT ALL", not "no token".
//
// # Why this can refuse to serve — and why a method alone never does
//
// A repo whose credential cannot be moved is a repo whose credential is not
// recoverable — it will fail at disaster time instead of now. Refusing makes
// that visible while it is still fixable. The blast radius is deliberately
// narrow: only a repo that ACTUALLY holds a credential can be refused, so
// SSH, public, local and origin-less repos are never affected by a missing
// agent key. A method-only carry keeps that promise: it needs no Crypt
// (SetOriginCredential encrypts only a non-empty token), and if it fails
// anyway there is no secret at stake, so it logs and serves the repo.
//
// Returning an error means "do not serve this repo". Returning nil means it is
// migrated, or had nothing to migrate.
//
// # What this gate does NOT cover
//
// It reads the STORE's auth columns, so it can only ever fire for a repo that
// has not been migrated yet. A repo created after control.db took ownership
// writes its credential straight there and leaves the store's columns empty —
// so this gate sees "nothing to carry" and serves it, no matter what state the
// control.db credential is in. An undecryptable control.db credential (rotated
// or replaced agent key) is therefore NOT caught here, and must not be assumed
// to be: verified on a live server, such a repo serves reads normally while
// Manager.OriginAuth fails its resolution and the reconcile records
// last_status="error" with the decrypt cause. That propagation — not this gate
// — is what keeps the failure from becoming a silent anonymous sync, and it is
// deliberate: the repo's facts stay readable, and only its syncing is broken.
//
// So read this gate as "no repo carries an unmigrated credential into service",
// which is all it can enforce — not as "no repo is ever served with a credential
// that cannot be used".
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
	if method == "" && token == "" {
		return nil // nothing to carry at all
	}

	if err := reg.SetOriginCredential(name, method, token); err != nil {
		if token == "" {
			// A method-only row holds NO secret, so this failure cannot lose
			// one and must never take a repo out of service — the blast-radius
			// promise above is what licenses the whole gate. ERROR, not WARN,
			// and it names the CONSEQUENCE rather than the failed call: the
			// method is the only thing that would have made this origin resolve
			// as anything other than anonymous.
			log.Error().Err(err).Str("repo", name).Str("auth_method", method).
				Msgf("repo %q names auth method %q in its own database and that method could not be "+
					"recorded in control.db, which is the only place origin auth is read from."+
					"\nThe repo is still served, because a method is not a secret and nothing is at"+
					" risk of being lost."+
					"\nBut its origin auth may now resolve ANONYMOUSLY (the URL alone re-derives a"+
					" method only for git@ and ssh:// origins), so a sync that should fail on a"+
					" missing credential can instead succeed against a remote that permits anonymous"+
					" access. Re-authenticate the origin to restore it.",
					name, method)
			return nil
		}
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
	// with_token distinguishes the two shapes without ever naming the secret:
	// false is the method-only carry, which needs no key and risks nothing.
	log.Info().Str("repo", name).Str("auth_method", method).Bool("with_token", token != "").
		Msg("origin auth migrated into control.db")
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
// SetOriginCredential updates the row whose name matches and whose archive_id is
// the empty string (gofmt's doc formatter mangles a literal two-quote SQL empty
// string, so it is spelled out) — and it errors
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
