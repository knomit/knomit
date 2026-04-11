// Replay algorithm: copies local facts into a target store's agent branch.
// Used when two knomit instances with disjoint histories connect.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// FactRow holds the minimal fields needed to replay a fact into another store.
type FactRow struct {
	Path       string
	BlobHash   string
	CommitHash string
}

// FactsIter is a cursor-based iterator over the branch_facts table. It yields
// facts one at a time, newest-first (by fact_id DESC), deduplicating by path
// so that only the latest version of each fact is returned. It never loads all
// facts into memory.
type FactsIter struct {
	rows *sql.Rows
	seen map[string]struct{}
}

// Next returns the next unique fact, or nil when iteration is complete.
// It skips paths that have already been yielded (dedup).
func (it *FactsIter) Next() (*FactRow, error) {
	for it.rows.Next() {
		var row FactRow
		if err := it.rows.Scan(&row.Path, &row.BlobHash, &row.CommitHash); err != nil {
			return nil, err
		}
		if _, dup := it.seen[row.Path]; dup {
			continue
		}
		it.seen[row.Path] = struct{}{}
		return &row, nil
	}
	return nil, it.rows.Err()
}

// Close releases the underlying database cursor. It is safe to call multiple times.
func (it *FactsIter) Close() error {
	if it.rows != nil {
		err := it.rows.Close()
		it.rows = nil
		return err
	}
	return nil
}

// Replay copies local facts into a target service's agent branch.
// It iterates facts from the iterator (newest-first, deduped by path), reads their
// blob content from the local store, resolves dead refs, and writes each fact
// to the target store.
func Replay(ctx context.Context, local *Service, localBranch string, iter FactIter, target *Service, cfg ReplayConfig) (*ReplayResult, error) {
	if cfg.AgentBranch == "" {
		return nil, fmt.Errorf("Replay: AgentBranch must be set")
	}
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = "main"
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyLocalWins
	}

	// 1. Set up agent branch in target store.
	agentRefName := plumbing.NewBranchReferenceName(cfg.AgentBranch)
	existingAgentRef, err := target.rh.gits.Reference(agentRefName)
	if cfg.UseExistingBranch && err == nil && existingAgentRef != nil {
		// Agent branch exists on remote and caller wants to reuse it — switch to it.
		if err := target.rh.gits.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); err != nil {
			return nil, fmt.Errorf("Replay: set HEAD to existing agent branch: %w", err)
		}
		log.Debug().Str("agent_branch", cfg.AgentBranch).Msg("replay: using existing remote agent branch as base")
	} else {
		// No existing agent branch — create from the selected main branch.
		defaultRefName := plumbing.NewBranchReferenceName(cfg.DefaultBranch)
		defaultRef, err := target.rh.gits.Reference(defaultRefName)
		if err != nil {
			return nil, fmt.Errorf("Replay: resolve default branch %q: %w", cfg.DefaultBranch, err)
		}
		if err := target.rh.gits.SetReference(plumbing.NewHashReference(agentRefName, defaultRef.Hash())); err != nil {
			return nil, fmt.Errorf("Replay: create agent branch: %w", err)
		}
		if err := target.rh.gits.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); err != nil {
			return nil, fmt.Errorf("Replay: set HEAD: %w", err)
		}
		log.Debug().Str("agent_branch", cfg.AgentBranch).Str("from", cfg.DefaultBranch).Msg("replay: created agent branch from main")
	}

	// 2. Collect all facts from the iterator for progress reporting.
	defer iter.Close()
	type localFact struct {
		path     string
		blobHash string
	}
	var facts []localFact
	for {
		row, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("Replay: iterate facts: %w", err)
		}
		if row == nil {
			break
		}
		facts = append(facts, localFact{path: row.Path, blobHash: row.BlobHash})
	}

	// 3. Count remote facts for stats.
	remotePaths, err := target.rh.ListAll(ctx, cfg.AgentBranch)
	if err != nil {
		return nil, fmt.Errorf("Replay: list remote facts: %w", err)
	}
	remotePathSet := make(map[string]bool, len(remotePaths))
	for _, p := range remotePaths {
		remotePathSet[p] = true
	}

	// Also build a set of local fact paths for dead ref resolution.
	localPathSet := make(map[string]bool, len(facts))
	for _, f := range facts {
		localPathSet[f.path] = true
	}

	result := &ReplayResult{
		TotalFacts: len(facts) + len(remotePaths),
		FromRemote: len(remotePaths),
	}

	// 4. For each local fact, replay into target.
	for i, f := range facts {
		// Read blob content from local git store.
		content, err := readBlobByHash(local, f.blobHash)
		if err != nil {
			return nil, fmt.Errorf("Replay: read blob %s for %s: %w", f.blobHash, f.path, err)
		}

		// Check if path exists in target (shared path).
		isShared := remotePathSet[f.path]
		if isShared {
			switch cfg.Strategy {
			case StrategyRemoteWins:
				// Skip — remote version is kept.
				if cfg.OnProgress != nil {
					cfg.OnProgress(i+1, len(facts))
				}
				continue
			case StrategyLocalWins:
				result.Overwrites++
			}
		}

		// Resolve dead refs.
		resolvedContent, resolvedCount, droppedCount, err := resolveDeadRefs(ctx, local, localBranch, content, f.path, localPathSet, remotePathSet)
		if err != nil {
			log.Warn().Err(err).Str("path", f.path).Msg("replay: dead ref resolution failed, using original content")
			resolvedContent = content
		}
		result.RefsResolvedFromHist += resolvedCount
		result.DanglingRefsDropped += droppedCount

		// Write fact to target store and commit.
		msg := fmt.Sprintf("replay: %s", f.path)
		if _, err := target.fi.WriteFact(ctx, cfg.AgentBranch, f.path, resolvedContent, msg, "replay"); err != nil {
			return nil, fmt.Errorf("Replay: write %s to target: %w", f.path, err)
		}
		result.FromLocal++

		if cfg.OnProgress != nil {
			cfg.OnProgress(i+1, len(facts))
		}
	}

	// Subtract overwrites: paths counted in both local and remote are only one
	// distinct fact in the target after replay.
	result.TotalFacts = result.TotalFacts - result.Overwrites

	log.Info().
		Int("total_facts", result.TotalFacts).
		Int("from_local", result.FromLocal).
		Int("from_remote", result.FromRemote).
		Int("overwrites", result.Overwrites).
		Int("refs_resolved", result.RefsResolvedFromHist).
		Int("refs_dropped", result.DanglingRefsDropped).
		Msg("replay complete")

	return result, nil
}

// readBlobByHash reads the content of a blob by its hash from the service's git repo.
func readBlobByHash(s *Service, hashStr string) (string, error) {
	h := plumbing.NewHash(hashStr)
	blob, err := s.rh.repo.BlobObject(h)
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: read: %w", err)
	}
	return string(b), nil
}

// resolveDeadRefs checks each ref in a fact's frontmatter and resolves dead local refs.
// Returns: modified content, count of resolved refs, count of dropped refs.
func resolveDeadRefs(ctx context.Context, local *Service, localBranch, content, path string, localPathSet, remotePathSet map[string]bool) (string, int, int, error) {
	f, err := fact.ParseFact(path, content)
	if err != nil {
		// Not a valid fact file — return as-is.
		return content, 0, 0, nil
	}

	if len(f.Refs) == 0 {
		return content, 0, 0, nil
	}

	var newRefs []string
	resolvedCount := 0
	droppedCount := 0

	for _, ref := range f.Refs {
		// External URL — always keep.
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			newRefs = append(newRefs, ref)
			continue
		}

		// Local path that exists in local store or target — keep.
		if localPathSet[ref] || remotePathSet[ref] {
			newRefs = append(newRefs, ref)
			continue
		}

		// Dead local ref — try to resolve from history (1 level deep).
		externalRefs, err := extractExternalRefsFromHistory(ctx, local, localBranch, ref)
		if err != nil {
			log.Debug().Err(err).Str("ref", ref).Str("fact", path).Msg("replay: could not resolve dead ref from history")
			droppedCount++
			continue
		}

		if len(externalRefs) == 0 {
			droppedCount++
			continue
		}

		// Graft external refs onto the current fact.
		newRefs = append(newRefs, externalRefs...)
		resolvedCount += len(externalRefs)
	}

	if resolvedCount == 0 && droppedCount == 0 {
		return content, 0, 0, nil
	}

	f.Refs = newRefs
	return fact.SerializeFact(f), resolvedCount, droppedCount, nil
}

// extractExternalRefsFromHistory looks up the last version of a deleted fact in
// local git history and extracts its external (http/https) refs.
func extractExternalRefsFromHistory(ctx context.Context, local *Service, localBranch, deadPath string) ([]string, error) {
	localHash, err := local.rh.resolveRef(ctx, localBranch)
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: ref: %w", err)
	}

	logIter, err := local.rh.repo.Log(&gogit.LogOptions{
		From:     localHash,
		FileName: &deadPath,
	})
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: log: %w", err)
	}
	defer logIter.Close()

	var deadContent string
	for {
		c, err := logIter.Next()
		if err != nil {
			return nil, fmt.Errorf("extractExternalRefsFromHistory: no commit with content found for %q", deadPath)
		}
		tree, err := c.Tree()
		if err != nil {
			continue
		}
		f, err := tree.File(deadPath)
		if err != nil {
			continue
		}
		deadContent, err = f.Contents()
		if err != nil {
			continue
		}
		break
	}

	deadFact, err := fact.ParseFact(deadPath, deadContent)
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: parse frontmatter: %w", err)
	}

	var externalRefs []string
	for _, ref := range deadFact.Refs {
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			externalRefs = append(externalRefs, ref)
		}
	}
	return externalRefs, nil
}
