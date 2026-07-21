package repos

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// rewireStore re-applies the per-store wiring that store.Open does NOT restore
// on its own — the embedder, credential encryption (Crypt), and the commit
// signer — to a freshly reopened Service. SwapStore must call this on every
// reopen; otherwise the swapped-in store silently loses the ability to store
// auth tokens (SetRemote refuses without a Crypt — the origin-connect "save
// remote config" failure) and to sign commits. Mirrors repoBuilder.openStore /
// configureCrypt / SetSigner so a swapped store behaves identically to a freshly
// built one.
func (m *Manager) rewireStore(svc *store.Service, repoName string) {
	if m.deps.Embedder != nil {
		svc.SetEmbedder(m.deps.Embedder)
	}
	configureCrypt(svc, m.deps.KeyPath, repoName)
	svc.SetSigner(m.deps.Signer)
	// store.Open does not restore the network timeout either; re-apply it so a
	// swapped-in store bounds remote git ops identically to a freshly built one.
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
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
		m.rewireStore(svc, ri.name)
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
		m.rewireStore(svc, ri.name)
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

	// Clean up backup — swap succeeded.
	os.Remove(backupPath)
	return nil
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
