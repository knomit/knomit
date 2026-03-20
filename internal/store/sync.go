// Git sync: keeps the search index up to date with the git-backed fact store.
// Supports both full rebuilds (first run) and incremental updates (diffing
// since the last indexed commit).
package store

import (
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

// Sync brings the index up to date with the git store.
//
// Algorithm:
//  1. Read meta.last_commit.
//  2. If missing → full rebuild (ListAll, index everything).
//  3. If last_commit == HEAD → no-op.
//  4. Else → DiffFiles(last_commit), upsert added+modified, delete removed.
//  5. Update meta.last_commit = HEAD.
func (idx *Index) Sync(git GitReader, branch string) error {
	head, err := git.HeadCommit()
	if err != nil {
		return fmt.Errorf("sync: head commit: %w", err)
	}

	last, err := idx.GetLastCommit(branch)
	if err != nil {
		return fmt.Errorf("sync: get last commit: %w", err)
	}

	if last == head {
		log.Debug().Str("head", head[:8]).Msg("index sync: already at HEAD, skipping")
		return nil
	}

	if last == "" {
		// Full rebuild: no previous commit recorded, so index every file.
		log.Info().Str("head", head[:8]).Msg("index sync: full rebuild (no previous commit)")
		paths, err := git.ListAll()
		if err != nil {
			return fmt.Errorf("sync: list all: %w", err)
		}
		for _, path := range paths {
			if err := idx.indexFile(git, path, head); err != nil {
				return err
			}
		}
		log.Info().Int("files", len(paths)).Msg("index sync: full rebuild complete")
	} else {
		// Incremental update: only process files changed since last_commit.
		added, modified, deleted, err := git.DiffFiles(last)
		if err != nil {
			return fmt.Errorf("sync: diff files: %w", err)
		}
		log.Debug().
			Str("from", last[:8]).Str("to", head[:8]).
			Int("added", len(added)).Int("modified", len(modified)).Int("deleted", len(deleted)).
			Msg("index sync: incremental update")
		for _, path := range append(added, modified...) {
			if err := idx.indexFile(git, path, head); err != nil {
				return err
			}
		}
		for _, path := range deleted {
			if err := idx.Delete(path); err != nil {
				return fmt.Errorf("sync: delete %q: %w", path, err)
			}
		}
	}

	return idx.SetLastCommit(branch, head)
}

// RebuildProgress is called during Rebuild to report progress.
type RebuildProgress func(phase string, done, total int)

// Rebuild clears the last-commit marker and re-indexes every file from HEAD
// using three phases: facts, embeddings, graph.
func (idx *Index) Rebuild(git GitReader, branch string, progress RebuildProgress) error {
	if err := idx.SetLastCommit(branch, ""); err != nil {
		return fmt.Errorf("rebuild: clear last commit: %w", err)
	}

	head, err := git.HeadCommit()
	if err != nil {
		return fmt.Errorf("rebuild: head commit: %w", err)
	}

	// Phase 1: facts
	start := time.Now()
	n, err := idx.rebuildFacts(git, head, progress)
	if err != nil {
		return fmt.Errorf("rebuild: facts: %w", err)
	}
	log.Info().Int("facts", n).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 1 (facts) complete")

	// Phase 2: embeddings
	start = time.Now()
	embedded, err := idx.rebuildEmbeddings(progress)
	if err != nil {
		return fmt.Errorf("rebuild: embeddings: %w", err)
	}
	log.Info().Int("embedded", embedded).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 2 (embeddings) complete")

	// Phase 3: graph
	start = time.Now()
	graphed, err := idx.rebuildGraph(progress)
	if err != nil {
		return fmt.Errorf("rebuild: graph: %w", err)
	}
	log.Info().Int("graphed", graphed).Str("elapsed", fmt.Sprintf("%.1fs", time.Since(start).Seconds())).Msg("rebuild: phase 3 (graph) complete")

	return idx.SetLastCommit(branch, head)
}

// indexFile reads a single file from git, parses it as a fact, and upserts
// it into the index. Files that fail to parse (e.g. kb.md manifest) are
// silently skipped.
//
// commitHash is the fallback; if commit_log has a more specific last-touch
// commit for this path, that is used instead.
func (idx *Index) indexFile(git GitReader, path, commitHash string) error {
	content, blobHash, err := git.ReadFileWithHash(path)
	if err != nil {
		return fmt.Errorf("indexFile: read %s: %w", path, err)
	}

	// Use the most recent non-merge commit that touched this file.
	if last, lerr := git.LastCommitForPath(path); lerr == nil && last != "" {
		commitHash = last
	}

	rec, err := parseFact(path, content, commitHash)
	if err != nil {
		return nil // not a fact file (e.g. kb.md manifest, ontology.yaml)
	}
	rec.BlobHash = blobHash

	return idx.Upsert(rec)
}
