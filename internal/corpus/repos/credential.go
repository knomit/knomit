package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
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

// CredentialChange is a new credential for a repo's origin: a pasted one
// (AuthMethod plus AuthToken, basic as user:password) or a link to a stored
// secret (SecretID).
type CredentialChange struct {
	AuthMethod string
	AuthToken  string
	SecretID   string
}

var (
	// ErrNoOrigin: a credential change on a repo that has no origin.
	ErrNoOrigin = errors.New("this repository has no origin: attach one with PUT /repos/{repo}/origin")
	// ErrLinkedRepo: a pasted credential for a repo linked to a stored secret.
	ErrLinkedRepo = errors.New("this repository uses a stored secret: unlink it first (PUT /repos/{repo}/origin/credential with \"secret_id\": null)")
	// ErrInvalidCredential: the credential is malformed or does not fit the
	// origin URL.
	ErrInvalidCredential = errors.New("invalid credential")
)

// beforeCredentialWrite (tests only) runs between the read of the stored
// origin and the compare-and-set that writes the new credential.
var beforeCredentialWrite func()

// afterCredentialReread (tests only) runs between the re-read of the written
// row and the in-memory write.
var afterCredentialReread func()

// checkPastedCredential validates a pasted credential against the stored url.
func checkPastedCredential(url string, c CredentialChange) error {
	switch c.AuthMethod {
	case "token":
		if c.AuthToken == "" {
			return fmt.Errorf("%w: auth_method token needs a token", ErrInvalidCredential)
		}
	case "basic":
		if user, pass, ok := strings.Cut(c.AuthToken, ":"); !ok || user == "" || pass == "" {
			return fmt.Errorf("%w: auth_method basic needs a user and a password", ErrInvalidCredential)
		}
	case "ssh", "none":
		if c.AuthToken != "" {
			return fmt.Errorf("%w: auth_method %s takes no token", ErrInvalidCredential, c.AuthMethod)
		}
	case "cert":
		return fmt.Errorf("%w: %w", ErrInvalidCredential, ErrCertNotAnInput)
	default:
		return fmt.Errorf("%w: auth_method must be token, basic, ssh or none", ErrInvalidCredential)
	}
	if err := MethodMismatch(url, c.AuthMethod); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidCredential, err.Error())
	}
	return nil
}

// ChangeOriginCredential replaces the credential of ri's origin WITHOUT the
// attach lifecycle: no probe, no guard, no git-remote rewrite, no Sync stage
// restart. The URL, branch and Mode stay exactly as stored.
//
// The change is one of: a link to a stored secret (c.SecretID set: the
// secret must fit the stored URL's host and scheme, ErrSecretHostNotAllowed
// or ErrSecretTypeMismatch; the stored method becomes the secret's
// transport and the pasted token is cleared); a pasted credential (c
// AuthMethod plus AuthToken), which on a linked repo needs unlink; or, with
// unlink and no credential, the explicit none (method "none", anonymous).
// The caller gates linking and unlinking (admin); this does not.
//
// It reads the stored origin and refuses: none (ErrNoOrigin), a fleet peer
// (ErrPeerCredential: a peer authenticates with this instance's certificate),
// a pasted credential for a linked repo unless unlink is set (ErrLinkedRepo),
// and a credential that is malformed or does not fit the URL
// (ErrInvalidCredential). It then writes with Origins.SetCredential, a
// compare-and-set on the stored URL, so an attach that re-points the origin
// in between wins and this answers control.ErrOriginChanged with nothing
// written. On success it re-reads the row and injects it into the running
// store with svc.Remote().SetOriginIf, a second compare-and-set on the whole
// injected origin as it was before the durable write: an attach, re-attach
// or detach that lands after the durable write keeps memory (and the durable
// row it wrote), and this answers
// control.ErrOriginChanged without signalling anything. Otherwise it signals
// a credential change, which resets the breakers and wakes the sync loop.
func (m *Manager) ChangeOriginCredential(ctx context.Context, ri *RepoInstance, c CredentialChange, unlink bool) error {
	origins := m.Origins()
	if origins == nil {
		return ErrManagerStopped
	}
	uid := ri.UID()
	stored, err := origins.Get(uid)
	if err != nil {
		return err
	}
	if stored == nil {
		return ErrNoOrigin
	}
	if stored.AuthMethod == "cert" {
		return ErrPeerCredential
	}
	switch {
	case c.SecretID != "":
		if c.AuthToken != "" {
			return fmt.Errorf("%w: secret_id takes no pasted credential", ErrInvalidCredential)
		}
		method, err := m.CheckSecretForURL(c.SecretID, stored.URL)
		if err != nil {
			return err
		}
		c.AuthMethod = method
	case stored.SecretID != "" && !unlink:
		return ErrLinkedRepo
	case unlink && c.AuthMethod == "" && c.AuthToken == "":
		// The explicit none: the repo is left anonymous.
		c.AuthMethod = "none"
	default:
		if err := checkPastedCredential(stored.URL, c); err != nil {
			return err
		}
	}
	// What the running store holds now, before the durable write: the
	// in-memory write below applies only while memory still holds exactly
	// this. With the store not open, the row an open would inject.
	expect := remotesync.Origin{URL: stored.URL, Branch: stored.Branch, AuthMethod: stored.AuthMethod, AuthToken: stored.AuthToken, SecretID: stored.SecretID}
	_ = ri.WithRead(func(svc *store.Service) {
		if mem := svc.Remote().InjectedOrigin(); mem != nil {
			expect = *mem
		}
	})
	if beforeCredentialWrite != nil {
		beforeCredentialWrite()
	}
	if err := origins.SetCredential(uid, stored.URL, c.AuthMethod, c.AuthToken, c.SecretID); err != nil {
		return err
	}
	cur, err := origins.Get(uid)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("change credential: %w: the origin was removed", control.ErrOriginChanged)
	}
	if afterCredentialReread != nil {
		afterCredentialReread()
	}
	// The running store's copy, as a compare-and-set on the WHOLE injected
	// origin read before the durable write. An attach or detach (even a
	// re-attach to the same URL) can land between the re-read and this write:
	// it writes control.db, the git remote and memory, and this must never
	// put cur back over it. The reconcile loop builds auth from memory while
	// go-git dials the git remote's URL, so an unconditional write here would
	// send the old origin's credential to the attached host, or resurrect a
	// detached origin. A store that is not open needs no write: opening
	// injects the control.db row.
	applied := true
	if werr := ri.WithRead(func(svc *store.Service) {
		applied = svc.Remote().SetOriginIf(expect, &remotesync.Origin{
			URL: cur.URL, Branch: cur.Branch, AuthMethod: cur.AuthMethod, AuthToken: cur.AuthToken, SecretID: cur.SecretID,
		})
	}); werr != nil {
		log.Debug().Err(werr).Str("repo", ri.Name()).Msg("credential changed while the store is not open; it is injected at open")
	}
	if !applied {
		return fmt.Errorf("change credential: %w: the origin was changed concurrently while the credential was being replaced", control.ErrOriginChanged)
	}
	ri.credentialChanged()
	return nil
}
