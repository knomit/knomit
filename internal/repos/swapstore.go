package repos

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// SwapStore replaces the repo's SQLite database with the one from tempDBPath.
// It stops sync loops, closes the old DB, copies the temp file over the real
// one, and reopens store.Service from the real path.
// If DBPath is empty (in-memory/test), it falls back to a pointer swap.
func (m *Manager) SwapStore(ri *RepoInstance, tempDBPath string) error {
	// Stop existing sync loops so no goroutines reference the old store.
	ri.mu.RLock()
	cancel := ri.syncCancel
	ri.mu.RUnlock()
	if cancel != nil {
		cancel()
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
		if m.deps.Embedder != nil {
			svc.SetEmbedder(m.deps.Embedder)
		}
		ri.withWrite(func() {
			ri.svc = svc
		})
		if ri.onCommit != nil {
			svc.SetOnCommit(ri.onCommit)
		}
		broadcastHead(svc, ri.agentBranch, ri.hub)
		return nil
	}

	// Close the old database.
	if ri.svc != nil {
		ri.svc.Close()
	}

	// Copy temp DB over the real one (backup first).
	backupPath := ri.dbPath + ".bak"
	if err := copyFile(ri.dbPath, backupPath); err != nil {
		// Non-fatal: we can proceed without a backup.
		log.Warn().Err(err).Msg("SwapStore: backup failed")
	}
	if err := copyFile(tempDBPath, ri.dbPath); err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.dbPath)
		return fmt.Errorf("SwapStore: copy temp DB: %w", err)
	}

	// Reopen from the real path.
	svc, err := store.Open(ri.dbPath)
	if err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.dbPath)
		return fmt.Errorf("SwapStore: reopen store: %w", err)
	}

	if err := svc.OpenRepo(); err != nil {
		svc.Close()
		_ = copyFile(backupPath, ri.dbPath)
		return fmt.Errorf("SwapStore: reopen git: %w", err)
	}

	if m.deps.Embedder != nil {
		svc.SetEmbedder(m.deps.Embedder)
	}
	ri.withWrite(func() {
		ri.svc = svc
	})
	if ri.onCommit != nil {
		svc.SetOnCommit(ri.onCommit)
	}
	broadcastHead(svc, ri.agentBranch, ri.hub)

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
