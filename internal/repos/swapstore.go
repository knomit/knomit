package repos

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// rewireStore re-applies EVERY piece of per-store wiring that store.Open does
// not restore on its own, to a freshly reopened Service: the embedder, the
// commit signer, the network timeout, the ontology root, and the injected
// origin. SwapStore must call this on every reopen — success path and recovery
// path alike — so a swapped store behaves identically to a freshly built one
// (mirrors repoBuilder.openStore).
//
// This list is meant to be COMPLETE. It previously read as complete while
// silently omitting the origin, and the omission only bit on the recovery
// paths: on success the caller re-injects, but a failed copy/reopen/git-open
// goes reattach(reopenLocal()) → the handler returns → nothing re-injects. The
// repo came back live with origin == nil, and the reconcile loop then hit
// sync.go's `if remote == nil { return }` and exited WITHOUT A LOG LINE. A repo
// that looks healthy and has quietly stopped syncing.
//
// Credential ENCRYPTION is genuinely not here: since migration 000017 the store
// holds no credentials, so there is no Crypt to restore — control.db's Origins
// owns the token and its Crypt, and both outlive the swap. The origin VALUES
// come from that same Origins tenant, which is why re-injecting them is a
// lookup rather than something the swap has to carry.
func (m *Manager) rewireStore(ri *RepoInstance, svc *store.Service) {
	if m.deps.Embedder != nil {
		svc.SetEmbedder(m.deps.Embedder)
	}
	svc.SetSigner(m.deps.Signer)
	// store.Open does not restore the network timeout either; re-apply it so a
	// swapped-in store bounds remote git ops identically to a freshly built one.
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	svc.SetOntologyRoot(m.deps.Cfg.OntologyRoot)
	m.reinjectOrigin(ri, svc)
}

// reinjectOrigin restores the control.db-owned connection identity onto a
// freshly opened Service. store.Open knows nothing about it: since 000017 the
// repo's own `remotes` row carries sync/push status only.
func (m *Manager) reinjectOrigin(ri *RepoInstance, svc *store.Service) {
	if ri == nil || ri.uid == "" || m.origins == nil {
		return
	}
	o, err := m.origins.Get(ri.uid)
	if err != nil {
		log.Warn().Err(err).Str("repo", ri.name).Str("uid", ri.uid).
			Msg("SwapStore: could not read the stored origin; sync stays inactive until the next boot")
		return
	}
	if o == nil {
		return
	}
	svc.SetOrigin(&store.Origin{
		URL:        o.URL,
		Branch:     o.Branch,
		AuthMethod: o.AuthMethod,
		AuthToken:  o.AuthToken,
	})
}

// SwapStore replaces the repo's SQLite database with the one from tempDBPath.
// It stops sync loops, closes the old DB, copies the temp file over the real
// one, and reopens store.Service from the real path.
// If DBPath is empty (in-memory/test), it falls back to a pointer swap.
func (m *Manager) SwapStore(ri *RepoInstance, tempDBPath string) error {
	// Stop the background index heal AND existing sync loops so no goroutines
	// reference the old store. indexWg before syncWg (the heal's activate() may
	// have started the loop), and both before the DB below is closed/swapped.
	ri.mu.RLock()
	cancel := ri.syncCancel
	indexCancel := ri.indexCancel
	ri.mu.RUnlock()
	if indexCancel != nil {
		indexCancel()
	}
	if cancel != nil {
		cancel()
	}
	if ri.indexWg != nil {
		ri.indexWg.Wait()
	}
	if ri.syncWg != nil {
		ri.syncWg.Wait()
	}

	// In-memory / test mode: no persistent DB file, open the temp DB directly.
	// The temp dir must NOT be cleaned up in this case (caller's responsibility).
	if ri.dbPath == "" {
		svc, err := store.Open(tempDBPath)
		if err != nil {
			// Fallback: no file-based swap possible. Keep existing svc.
			log.Warn().Err(err).Msg("SwapStore: cannot open temp DB, keeping existing service")
			return nil
		}
		if err := svc.OpenRepo(); err != nil {
			svc.Close()
			log.Warn().Err(err).Msg("SwapStore: cannot open temp git, keeping existing service")
			return nil
		}
		m.rewireStore(ri, svc)
		if ri.onCommit != nil {
			svc.SetOnCommit(ri.onCommit)
		}
		// Detach the old generation and install the new one, then drain the old
		// handle's in-flight users (Acquire holders: MCP tool calls, SSE flows,
		// the session reaper, background rebuilds) before closing it. Detaching
		// under the write lock guarantees no NEW user can reach the old service;
		// the refcount drain covers users that acquired earlier and are still
		// mid-operation — the case a bare lock barrier cannot cover. Closing the
		// old Service (rather than dropping it) also releases its ephemeral
		// session DB handle and file, which would otherwise leak on every swap.
		old := ri.detachStore(false)
		if !ri.attachStore(svc) {
			// Instance was closed concurrently — the new service has no owner.
			svc.Close()
		}
		if old != nil {
			old.wg.Wait()
			old.svc.Close()
		}
		broadcastHead(svc, ri.agentBranch, ri.hub)
		m.recordSwappedIdentity(ri)
		return nil
	}

	// File-backed swap: detach the current handle so new Acquires fail fast
	// with ErrStoreUnavailable while the on-disk file is being replaced, drain
	// the in-flight users (Acquire holders — MCP tool calls, SSE flows, the
	// session reaper — that a lock barrier alone could not cover), close the
	// old DB, copy the temp file over the real one, and reopen.
	old := ri.detachStore(false)
	if old != nil {
		old.wg.Wait()
		old.svc.Close()
	}

	// reattach installs svc as the new generation on success paths; on failure
	// paths it is called with the service reopened from the restored backup so
	// the repo does not stay unavailable after a failed swap. A nil svc (the
	// recovery reopen itself failed) leaves the handle detached — Acquire keeps
	// returning ErrStoreUnavailable, which is the truthful state.
	reattach := func(svc *store.Service) {
		if svc == nil {
			return
		}
		m.rewireStore(ri, svc)
		if ri.onCommit != nil {
			svc.SetOnCommit(ri.onCommit)
		}
		if !ri.attachStore(svc) {
			svc.Close() // instance closed concurrently; nothing may own svc now
			return
		}
		broadcastHead(svc, ri.agentBranch, ri.hub)
	}
	// reopenLocal reopens from ri.dbPath after the backup restore; best-effort.
	reopenLocal := func() *store.Service {
		svc, err := store.Open(ri.dbPath)
		if err != nil {
			log.Error().Err(err).Str("repo", ri.name).Msg("SwapStore: recovery reopen failed; repo store unavailable")
			return nil
		}
		if err := svc.OpenRepo(); err != nil {
			svc.Close()
			log.Error().Err(err).Str("repo", ri.name).Msg("SwapStore: recovery git reopen failed; repo store unavailable")
			return nil
		}
		return svc
	}

	// Copy temp DB over the real one (backup first).
	backupPath := ri.dbPath + ".bak"
	if err := copyFile(ri.dbPath, backupPath); err != nil {
		// Non-fatal: we can proceed without a backup.
		log.Warn().Err(err).Msg("SwapStore: backup failed")
	}
	if err := copyFile(tempDBPath, ri.dbPath); err != nil {
		// Restore from backup and reopen so the repo stays usable.
		_ = copyFile(backupPath, ri.dbPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: copy temp DB: %w", err)
	}

	// Reopen from the real path.
	svc, err := store.Open(ri.dbPath)
	if err != nil {
		_ = copyFile(backupPath, ri.dbPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: reopen store: %w", err)
	}

	if err := svc.OpenRepo(); err != nil {
		svc.Close()
		_ = copyFile(backupPath, ri.dbPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: reopen git: %w", err)
	}

	reattach(svc)
	m.recordSwappedIdentity(ri)

	// Clean up backup — swap succeeded.
	os.Remove(backupPath)
	return nil
}

// recordSwappedIdentity re-reads the repo's root commit after a store swap and
// writes it to the registry.
//
// A swap replaces the knowledge base a repo holds — that is what a
// disjoint-history origin connect does — so repo_id must follow it. This lives
// here rather than in the web handler so EVERY swap path, present and future,
// maintains the invariant.
//
// Callers check uniqueness BEFORE swapping (the swap is a point of no return),
// so a conflict here means the pre-swap check was skipped or raced. Log it
// loudly; do not attempt to undo the swap.
func (m *Manager) recordSwappedIdentity(ri *RepoInstance) {
	if m.reg == nil || ri == nil || ri.uid == "" {
		return
	}
	// The cached id belongs to the previous generation of the store; clear it
	// so ID() re-resolves against the freshly attached one.
	ri.idMu.Lock()
	ri.id = ""
	ri.idMu.Unlock()

	id := ri.ID()
	if id == "" {
		log.Warn().Str("repo", ri.name).Msg("SwapStore: root commit unresolved; registry identity not updated")
		return
	}
	if err := m.reg.RecordRepoID(ri.uid, id); err != nil {
		log.Error().Err(err).Str("repo", ri.name).Str("uid", ri.uid).Str("repo_id", id).
			Msg("SwapStore: registry identity not updated; the pre-swap uniqueness check was skipped or raced")
	}
}

func broadcastHead(svc *store.Service, branch string, hub *TaskHub) {
	if hub == nil {
		return
	}
	if head, err := svc.Branches().HeadCommit(context.Background(), branch); err == nil {
		hub.broadcastStatus(head)
	}
}

// copyFile copies src to dst, creating dst if needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
