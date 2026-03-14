// Remote synchronization: fetches from origin and merges origin/main into the
// agent branch. The merge strategy is "origin wins" — files from origin/main
// are overlaid onto the agent tree.
package git

import (
	"fmt"
	"io"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/rs/zerolog/log"
)

// Sync fetches from origin and merges origin/main into the agent branch.
// If no remote exists, returns SyncResult{Synced: false}, nil.
func (s *Store) Sync(remoteAuth interface{}) (SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if origin remote exists.
	_, err := s.repo.Remote("origin")
	if err != nil {
		log.Debug().Msg("git sync: no origin remote configured, skipping")
		return SyncResult{Synced: false}, nil
	}

	// Fetch from origin.
	log.Debug().Msg("git sync: fetching from origin")
	err = s.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}
	if err == gogit.NoErrAlreadyUpToDate {
		log.Debug().Msg("git sync: fetch reports already up to date")
	}

	// Resolve origin/main ref.
	originMainRef, err := s.storer.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	if err != nil {
		log.Debug().Msg("git sync: origin/main ref not found, skipping")
		return SyncResult{Synced: false}, nil
	}
	originMainHash := originMainRef.Hash()

	// Get current agent branch HEAD.
	agentRefName := plumbing.NewBranchReferenceName(s.branch)
	agentRef, err := s.storer.Reference(agentRefName)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent ref: %w", err)
	}
	agentHash := agentRef.Hash()

	log.Debug().
		Str("origin_main", originMainHash.String()[:8]).
		Str("agent_head", agentHash.String()[:8]).
		Str("branch", s.branch).
		Msg("git sync: comparing refs")

	// Count commits in origin/main not in agent branch.
	ahead, err := s.countAhead(originMainHash, agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: count ahead: %w", err)
	}

	// If origin/main is already an ancestor of agent branch, nothing to merge.
	isAncestor, err := s.isAncestor(originMainHash, agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: check ancestor: %w", err)
	}
	if isAncestor {
		log.Debug().Int("ahead", ahead).Msg("git sync: origin/main already merged, nothing to do")
		return SyncResult{Synced: false, Ahead: ahead}, nil
	}

	// Create a merge commit: agent branch HEAD + origin/main as parents,
	// using origin/main's tree merged on top of agent's tree.
	// Strategy: create a merge commit with two parents, using the to-tree from origin/main
	// overlaid on the agent tree (origin/main wins for conflicts — simple strategy).
	originCommit, err := s.repo.CommitObject(originMainHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: origin commit: %w", err)
	}

	agentCommit, err := s.repo.CommitObject(agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent commit: %w", err)
	}

	// Build merged tree: start from agent tree, apply all files from origin/main tree.
	mergedTreeHash, err := s.mergeTrees(agentCommit, originCommit)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: merge trees: %w", err)
	}

	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	mergeCommit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      fmt.Sprintf("sync: merge origin/main into %s", s.branch),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{agentHash, originMainHash},
	}

	commitObj := s.storer.NewEncodedObject()
	if err := mergeCommit.Encode(commitObj); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: encode merge commit: %w", err)
	}
	mergeHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: store merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(agentRefName, mergeHash)
	if err := s.storer.SetReference(newRef); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: update ref: %w", err)
	}

	s.notifyCommit(mergeHash.String())

	log.Info().
		Int("ahead", ahead).
		Str("merge_commit", mergeHash.String()[:8]).
		Msg("git sync: merged origin/main")
	return SyncResult{Synced: true, Ahead: ahead}, nil
}

// countAhead returns the number of commits reachable from tip that are not
// reachable from base. It walks tip's history and stops when it hits a commit
// that is also reachable from base.
func (s *Store) countAhead(tip, base plumbing.Hash) (int, error) {
	// Collect all commits reachable from base.
	baseSet := make(map[plumbing.Hash]bool)
	baseIter, err := s.repo.Log(&gogit.LogOptions{From: base})
	if err != nil {
		return 0, err
	}
	defer baseIter.Close()
	_ = baseIter.ForEach(func(c *object.Commit) error {
		baseSet[c.Hash] = true
		return nil
	})

	count := 0
	tipIter, err := s.repo.Log(&gogit.LogOptions{From: tip})
	if err != nil {
		return 0, err
	}
	defer tipIter.Close()
	_ = tipIter.ForEach(func(c *object.Commit) error {
		if baseSet[c.Hash] {
			return io.EOF
		}
		count++
		return nil
	})
	return count, nil
}

// isAncestor returns true if candidate is an ancestor of (or equal to) tip.
// It walks tip's history looking for candidate.
func (s *Store) isAncestor(candidate, tip plumbing.Hash) (bool, error) {
	if candidate == tip {
		return true, nil
	}
	iter, err := s.repo.Log(&gogit.LogOptions{From: tip})
	if err != nil {
		return false, err
	}
	defer iter.Close()
	found := false
	_ = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == candidate {
			found = true
			return io.EOF
		}
		return nil
	})
	return found, nil
}

// mergeTrees creates a merged tree: starts from agentCommit's tree, then
// overlays all files from originCommit's tree (origin/main wins for conflicts).
func (s *Store) mergeTrees(agentCommit, originCommit *object.Commit) (plumbing.Hash, error) {
	originFileIter, err := originCommit.Files()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: origin files: %w", err)
	}
	defer originFileIter.Close()

	// Load agent root tree.
	agentTree, err := agentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: agent tree: %w", err)
	}

	currentTree := agentTree
	var currentRootHash plumbing.Hash

	err = originFileIter.ForEach(func(f *object.File) error {
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("mergeTrees: read origin file %q: %w", f.Name, err)
		}

		// Create blob.
		blobObj := s.storer.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return err
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return err
		}
		bw.Close()
		blobHash, err := s.storer.SetEncodedObject(blobObj)
		if err != nil {
			return err
		}

		currentRootHash, err = buildTree(s.storer, currentTree, f.Name, blobHash)
		if err != nil {
			return err
		}
		currentTree, err = object.GetTree(s.storer, currentRootHash)
		return err
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mergeTrees: overlay: %w", err)
	}

	if currentRootHash == plumbing.ZeroHash {
		// No files from origin — use agent tree hash.
		agentCommitObj, err := s.repo.CommitObject(agentCommit.Hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		t, err := agentCommitObj.Tree()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return t.Hash, nil
	}
	return currentRootHash, nil
}
