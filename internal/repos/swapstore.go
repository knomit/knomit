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
	svc.SetOntologyRoot(m.deps.Cfg.OntologyRoot)
}

// SwapStore replaces the repo's SQLite database with the one from tempDBPath.
// It stops sync loops, closes the old DB, copies the temp file over the real
// one, and reopens store.Service from the real path.
// If DBPath is empty (in-memory/test), it falls back to a pointer swap.
func (m *Manager) SwapStore(ri *RepoInstance, tempDBPath string) (err error) {
	// Pause replication FIRST — before the sync-loop teardown, and so before
	// the store is closed and the file replaced.
	//
	// Before, because the swapped-in file is a new identity: replicating across
	// the swap uploads deltas computed against a database that no longer
	// exists. Pausing also drops the replicator's SQLite connection ahead of
	// knomit's own, so knomit's is the LAST to close and SQLite checkpoints and
	// deletes the -wal — leaving no stale sidecar WAL to be replayed onto the
	// new file.
	//
	// resume runs from a DEFER, not from the success path, because a failed swap
	// that leaves the repo silently unreplicated is worse than the failure — and
	// the error it returns would never even mention backup.
	//
	// What makes a defer safe is that the file is SETTLED at every return below
	// it, never mid-swap. The replacement is staged in a sibling file and moved
	// in by rename, so each exit path leaves one of exactly two complete
	// databases at ri.dbPath:
	//
	//   - before the rename: the original, byte-for-byte the file Pause saw.
	//     Resuming replication of it is not a fallback, it is the right answer —
	//     nothing about it changed.
	//   - after the rename: the replacement, or the .bak renamed back over it.
	//
	// There is no window in which a torn or truncated file is visible to resume,
	// which is the property that stops a failed swap from being snapshotted as
	// the new replica head.
	if m.deps.Backup != nil {
		resume, perr := m.deps.Backup.Pause(ri.name)
		if perr != nil {
			return fmt.Errorf("SwapStore: pause replication: %w", perr)
		}
		defer func() {
			rerr := resume()
			if rerr == nil {
				return
			}
			rerr = fmt.Errorf("SwapStore: resume replication: %w", rerr)
			if err == nil {
				err = rerr
				return
			}
			// The swap already failed; keep that error (it is the cause) but do
			// not let the unreplicated repo pass unrecorded.
			log.Error().Err(rerr).Str("repo", ri.name).
				Msg("SwapStore: repo is no longer being replicated")
		}()
	}

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

	// Replace the live database by STAGING the replacement beside it and
	// renaming it into place. Never by copying onto it.
	//
	// The old shape — copy live→.bak best-effort, then copy temp→live — put the
	// live database into a state no reader and no replica should ever see. Any
	// copy onto a path opens it with O_TRUNC, so a failure part-way through
	// io.Copy left a truncated .db; a 0-byte file is a VALID empty SQLite
	// database, so the recovery reopen below succeeded, an empty store was
	// reattached, and — since this branch wired Pause/resume around the swap —
	// resume then re-anchored litestream against that empty file and made it the
	// replica head. Local copy, backup copy and replica all gone at once, with
	// nothing raising an error. A best-effort .bak is exactly the wrong shape
	// when it is the ONLY copy standing between a failed swap and total loss.
	//
	// So: the .bak is mandatory, the replacement is assembled somewhere it can be
	// abandoned, and the live file changes only by rename — atomic within the
	// directory, so it is either wholly the old database or wholly the new one.
	//
	// What this buys the deferred resume(): on every path that returns before the
	// rename, the live file is bit-for-bit what it was when Pause looked at it,
	// so resuming replication of the ORIGINAL is not merely safe but the right
	// outcome. On every path after it, the file is a complete database — the
	// replacement, or the .bak renamed back. resume() can no longer snapshot a
	// torn or empty file on ANY path.
	backupPath := ri.dbPath + ".bak"
	stagedPath := ri.dbPath + ".knomit-swap"

	if err := copyFile(ri.dbPath, backupPath); err != nil {
		// FATAL, where it used to be a warning. Proceeding without the backup is
		// proceeding with no way back, and the caller is told before anything has
		// been touched: the live database is still open-able and still the one
		// being replicated.
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: back up %s before replacing it: %w", ri.dbPath, err)
	}

	// A staged file from an interrupted earlier swap would be silently adopted by
	// the rename below, so clear it rather than trust it.
	if err := os.Remove(stagedPath); err != nil && !os.IsNotExist(err) {
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: clear %s left by an earlier swap: %w", stagedPath, err)
	}
	if err := copyFile(tempDBPath, stagedPath); err != nil {
		os.Remove(stagedPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: stage the replacement database at %s: %w", stagedPath, err)
	}

	// Sidecars BEFORE the rename, never after: a -wal surviving onto the new file
	// is replayed into it by SQLite on first open, and a WAL header carries no
	// database identity, so the corruption is silent. Closing the old service
	// above should already have checkpointed and removed them; this covers the
	// close that did not.
	if err := clearSQLiteSidecars(ri.dbPath); err != nil {
		os.Remove(stagedPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: %w", err)
	}
	if err := os.Rename(stagedPath, ri.dbPath); err != nil {
		os.Remove(stagedPath)
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: move the staged database into place at %s: %w", ri.dbPath, err)
	}

	// rollback puts the pre-swap database back, by rename for the same reason the
	// swap uses one: a copy back would truncate the live path first and could
	// then fail with nothing at all in place.
	rollback := func() {
		if err := clearSQLiteSidecars(ri.dbPath); err != nil {
			log.Error().Err(err).Str("repo", ri.name).Msg("SwapStore: rollback could not clear sidecars")
			return
		}
		if err := os.Rename(backupPath, ri.dbPath); err != nil {
			// The swapped-in database stays in place: it is complete, merely not
			// the one that was asked for. The .bak is still on disk under a name
			// this message gives the operator.
			log.Error().Err(err).Str("repo", ri.name).Str("backup", backupPath).
				Msg("SwapStore: rollback failed; the repo is serving the swapped-in database and the pre-swap copy is at the path above")
		}
	}

	// Reopen from the real path.
	svc, err := store.Open(ri.dbPath)
	if err != nil {
		rollback()
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: reopen store: %w", err)
	}

	if err := svc.OpenRepo(); err != nil {
		svc.Close()
		rollback()
		reattach(reopenLocal())
		return fmt.Errorf("SwapStore: reopen git: %w", err)
	}

	reattach(svc)

	// Clean up backup — swap succeeded.
	os.Remove(backupPath)
	return nil
}

// clearSQLiteSidecars removes the WAL, shared-memory and rollback-journal files
// beside a database that is about to be REPLACED.
//
// Deleting is the correct disposal, not just the convenient one: these files are
// deltas over the pages of the specific database file being replaced. Once it is
// gone there is nothing to apply them to and nothing in them is recoverable —
// whereas leaving one behind can only corrupt, because SQLite replays it into
// whatever file it finds under that name and a WAL header carries no identity to
// check against.
//
// A sidecar that cannot be removed is an error rather than something to swap
// around, for that reason. This mirrors the agent's clearOrphanedSidecars; it is
// duplicated rather than shared because internal/repos must never import
// internal/backup (see BackupTracker).
func clearSQLiteSidecars(dbPath string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", dbPath+suffix, err)
		}
	}
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
//
// It TRUNCATES dst before it knows whether the copy will succeed, so it must
// only ever be pointed at a scratch path — a .bak, or a staged file waiting to
// be renamed. Aimed at a live database it is a data-loss primitive: a failure
// part-way through leaves 0 bytes, which SQLite opens happily as an empty
// database and litestream will replicate as one.
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
	// Sync before returning, because the caller's next move is a rename. Rename
	// is atomic against other readers but not against a crash: without this, a
	// power loss can leave the destination name resolving to a length the data
	// never reached.
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}
