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
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil // the caller's own open handling covers an unusable store
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
