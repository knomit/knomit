package repos

import (
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog/log"

	"knomit/internal/git"
	"knomit/internal/store"
)

// SwapStore replaces the repo's SQLite database with the one from tempDBPath.
// It stops sync loops, closes the old DB, copies the temp file over the real
// one, and reopens store.Service + git.Store from the real path.
// If DBPath is empty (in-memory/test), it falls back to a pointer swap.
func (m *Manager) SwapStore(ri *RepoInstance, tempDBPath string) error {
	// Stop existing sync loops so no goroutines reference the old store.
	if ri.SyncCancel != nil {
		ri.SyncCancel()
	}
	if ri.SyncWg != nil {
		ri.SyncWg.Wait()
	}

	// In-memory / test mode: no persistent DB file, open the temp DB directly.
	// The temp dir must NOT be cleaned up in this case (caller's responsibility).
	if ri.DBPath == "" {
		svc, err := store.Open(tempDBPath)
		if err != nil {
			// Fallback: no file-based swap possible. Keep existing Svc.
			log.Warn().Err(err).Msg("SwapStore: cannot open temp DB, keeping existing service")
			return nil
		}
		gs, err := git.OpenWithStorer(svc.GitStorer())
		if err != nil {
			svc.Close()
			log.Warn().Err(err).Msg("SwapStore: cannot open temp git, keeping existing service")
			return nil
		}
		idx := svc.Index()
		if m.deps.Embedder != nil {
			idx.SetEmbedder(m.deps.Embedder)
		}
		ri.mu.Lock()
		ri.Svc = svc
		ri.GS = gs
		ri.Idx = idx
		ri.mu.Unlock()
		return nil
	}

	// Close the old database.
	if ri.Svc != nil {
		ri.Svc.Close()
	}

	// Copy temp DB over the real one (backup first).
	backupPath := ri.DBPath + ".bak"
	if err := copyFile(ri.DBPath, backupPath); err != nil {
		// Non-fatal: we can proceed without a backup.
		log.Warn().Err(err).Msg("SwapStore: backup failed")
	}
	if err := copyFile(tempDBPath, ri.DBPath); err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: copy temp DB: %w", err)
	}

	// Reopen from the real path.
	svc, err := store.Open(ri.DBPath)
	if err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: reopen store: %w", err)
	}

	gs, err := git.OpenWithStorer(svc.GitStorer())
	if err != nil {
		svc.Close()
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: reopen git: %w", err)
	}

	idx := svc.Index()
	if m.deps.Embedder != nil {
		idx.SetEmbedder(m.deps.Embedder)
	}
	ri.mu.Lock()
	ri.Svc = svc
	ri.GS = gs
	ri.Idx = idx
	ri.mu.Unlock()

	// Clean up backup — swap succeeded.
	os.Remove(backupPath)
	return nil
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
