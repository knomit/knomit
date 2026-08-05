package repos

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

// SetOrigin records a repo's origin and materializes it into the repo's store.
//
// # control.db is written FIRST, always
//
// The order is the whole point. control.db is the only copy that survives the
// loss of the repo database, so a crash between the two steps must leave the
// credential recorded and the store unwired — a state the next boot repairs by
// materializing. The reverse order leaves a repo syncing happily whose
// credential exists nowhere recoverable, which is the failure this design
// exists to remove.
//
// The store is told the url, branch and intervals but NEVER the credential:
// its auth columns stay empty from here on, so there is exactly one ciphertext
// and exactly one place to clear.
//
// ctx is threaded for the same reason every sibling write takes one — nothing
// below it takes a context yet (neither the registry nor store.RemoteIndex),
// so giving them one later needs no signature change here.
func (m *Manager) SetOrigin(ctx context.Context, name string, spec OriginSpec, interval, pushInterval int) error {
	reg := m.RepoRegistry()
	if reg == nil {
		return fmt.Errorf("set origin %q: registry unavailable", name)
	}
	ri := m.Get(name)
	if ri == nil {
		return fmt.Errorf("set origin %q: %w", name, ErrRepoNotFound)
	}

	// 1. control.db first.
	rec, found, err := reg.ActiveRecord(name)
	if err != nil {
		return fmt.Errorf("set origin %q: read registry: %w", name, err)
	}
	if !found {
		rec = RepoRecord{Name: name, State: RepoActive, CreatedAt: time.Now().UTC()}
	}
	rec.OriginURL = spec.URL
	rec.OriginBranch = spec.Branch
	if err := reg.Upsert(rec); err != nil {
		return fmt.Errorf("set origin %q: record origin: %w", name, err)
	}
	if err := reg.SetOriginCredential(name, spec.AuthMethod, spec.AuthToken); err != nil {
		return fmt.Errorf("set origin %q: record credential: %w", name, err)
	}

	// 2. Then materialize, with no secret.
	svc, release, err := ri.Acquire()
	if err != nil {
		return fmt.Errorf("set origin %q: acquire store: %w", name, err)
	}
	defer release()
	if err := svc.Remote().SetRemote("origin", spec.URL, spec.Branch, ri.AgentBranch(),
		interval, pushInterval, "", ""); err != nil {
		return fmt.Errorf("set origin %q: wire remote: %w", name, err)
	}
	return nil
}

// ClearOrigin disconnects a repo's origin and forgets its credential.
//
// # Same ordering rule as SetOrigin, and for the mirror-image reason
//
// Clearing the store first would leave control.db still holding BOTH the origin
// and its credential, and Manager.reconcileOrigin would then re-materialize the
// origin the user just disconnected — with the credential intact, so it would
// keep syncing, and every later boot would do it again.
//
// # What a crash between the two steps actually leaves, stated plainly
//
// NOT a clean state that the next boot tidies up. control.db has forgotten the
// origin and its credential, the store is still wired, and reconcileOrigin
// CANNOT tell a row the user just blanked apart from one control.db has not
// learned yet (adoptFromFilesystem writes exactly that shape). It resolves that
// ambiguity by adopting the store's origin — so the disconnected URL is RECORDED
// AGAIN in control.db and sync resumes against it.
//
// That is a deliberate trade, not an oversight: see reconcileOrigin for why
// unwiring blank rows would be worse. The recovery is to disconnect again, which
// costs the user nothing but the second call — and the credential does not come
// back with it, so an authenticated origin resurrects unusable rather than
// silently live.
func (m *Manager) ClearOrigin(ctx context.Context, name string) error {
	reg := m.RepoRegistry()
	if reg == nil {
		return fmt.Errorf("clear origin %q: registry unavailable", name)
	}

	// 1. control.db first.
	//
	// Both writes are inside the found branch: SetOriginCredential reports "no
	// active row" as an error, so calling it for an unregistered repo would
	// fail a disconnect that has nothing to forget in the first place.
	rec, found, err := reg.ActiveRecord(name)
	if err != nil {
		return fmt.Errorf("clear origin %q: read registry: %w", name, err)
	}
	if found {
		rec.OriginURL = ""
		rec.OriginBranch = ""
		if err := reg.Upsert(rec); err != nil {
			return fmt.Errorf("clear origin %q: clear origin row: %w", name, err)
		}
		if err := reg.SetOriginCredential(name, "", ""); err != nil {
			return fmt.Errorf("clear origin %q: clear credential: %w", name, err)
		}
	}

	// 2. Then unwire.
	ri := m.Get(name)
	if ri == nil {
		return nil // already gone; control.db is authoritative and now agrees
	}
	svc, release, err := ri.Acquire()
	if err != nil {
		return fmt.Errorf("clear origin %q: acquire store: %w", name, err)
	}
	defer release()
	if err := svc.Remote().DeleteRemote("origin"); err != nil {
		log.Warn().Err(err).Str("repo", name).
			Msg("clear origin: control.db has forgotten this origin and its credential, but the store is STILL " +
				"wired to it; the next boot will NOT remove that wiring — it will record the origin back into " +
				"control.db (a blank row is indistinguishable from one that has not learned yet). Disconnect " +
				"the origin again to clear it; the credential stays forgotten either way")
	}
	return nil
}

// OriginAuth resolves a repo's credential config from control.db.
//
// There is no fallback to the store's legacy auth columns, and that is a
// consequence of the boot migration gate rather than an oversight: a repo that
// still holds an unmigrated credential is never served, so any repo reaching
// this function has already had its credential moved. One place to read.
func (m *Manager) OriginAuth(name string) (config.RemoteAuthConfig, error) {
	cfg := m.deps.Cfg.Remote
	reg := m.RepoRegistry()
	if reg == nil {
		return cfg, nil
	}
	method, token, err := reg.OriginCredential(name)
	if err != nil {
		return cfg, err
	}
	if method != "" {
		cfg.AuthMethod = method
	}
	if token != "" {
		if method == "basic" {
			if user, pass, ok := strings.Cut(token, ":"); ok {
				cfg.User, cfg.Password = user, pass
			}
		} else {
			cfg.Token = token
		}
	}
	return cfg, nil
}
