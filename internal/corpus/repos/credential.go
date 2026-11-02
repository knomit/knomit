package repos

import (
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos/control"
)

// credentialChanged is what every credential change ends with: a stored
// secret's value replaced, a link or unlink, or a pasted-credential swap. It
// asks the origin loop to close its circuit breakers (the one scoped
// exception in breaker.go) and wakes it, so the new credential is tried at
// once instead of after an open period or the next tick. Nil-safe.
func (ri *RepoInstance) credentialChanged() {
	if ri == nil {
		return
	}
	ri.breakers.requestReset()
	ri.wakeSync()
}

// BreakerResetGenForTest reports how many credential changes this repo's
// breakers were asked to act on.
func (ri *RepoInstance) BreakerResetGenForTest() uint64 { return ri.breakers.resetGen() }

// errSecretsUnavailable: the secrets store is not open (before Start).
var errSecretsUnavailable = errors.New("the secrets store is not open")

// ReplaceSecretValue stores a new value for secret id (validated against its
// type) and then signals a credential change to every ACTIVE repo linked to
// it, so each one resets its breakers and syncs at once with the new value.
// Nothing restarts: the loops resolve the secret per operation.
func (m *Manager) ReplaceSecretValue(id string, v control.SecretValue) error {
	s := m.Secrets()
	if s == nil {
		return errSecretsUnavailable
	}
	if err := s.ReplaceValue(id, v, time.Now().UTC()); err != nil {
		return err
	}
	linked, err := s.LinkedRepos(id)
	if err != nil {
		// The value is stored; the linked repos pick it up on their next
		// round. Only the immediate wake is lost.
		log.Warn().Err(err).Str("secret_id", id).Msg("secrets: value replaced, but the linked repos could not be listed to wake them")
		return nil
	}
	for _, r := range linked {
		if r.State != string(control.StateActive) {
			continue
		}
		m.GetByUID(r.UID).credentialChanged()
	}
	return nil
}

