// Remote synchronization: fetches from origin and merges the remote branch
// into the agent branch using a common-ancestor-aware three-way merge with
// "origin wins" semantics.
package git

import (
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"

	"github.com/rs/zerolog/log"
)

// Sync fetches from origin and merges origin/<remoteBranch> into the agent
// branch using a three-way merge with "origin wins" semantics.
//
// Lock is held from fetch through ref update, then released before
// notifyCommit (which triggers index sync and may call back into Store).
//
// If remoteBranch is empty, it defaults to "main".
func (s *Store) Sync(remoteBranch string) (SyncResult, error) {
	if remoteBranch == "" {
		remoteBranch = "main"
	}

	s.mu.Lock()

	// Check if origin remote exists.
	_, err := s.repo.Remote("origin")
	if err != nil {
		s.mu.Unlock()
		log.Debug().Msg("git sync: no origin remote configured, skipping")
		return SyncResult{}, nil
	}

	// Fetch from origin.
	log.Debug().Msg("git sync: fetching from origin")
	err = s.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       s.auth,
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}

	// Resolve origin/<remoteBranch> ref.
	originRef, err := s.storer.Reference(plumbing.NewRemoteReferenceName("origin", remoteBranch))
	if err != nil {
		s.mu.Unlock()
		log.Debug().Str("branch", remoteBranch).Msg("git sync: origin ref not found, skipping")
		return SyncResult{}, nil
	}
	originHash := originRef.Hash()

	// Get current agent branch HEAD.
	agentRefName := plumbing.NewBranchReferenceName(s.branch)
	agentRef, err := s.storer.Reference(agentRefName)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: agent ref: %w", err)
	}
	agentHash := agentRef.Hash()

	log.Debug().
		Str("origin", originHash.String()[:8]).
		Str("agent", agentHash.String()[:8]).
		Str("branch", s.branch).
		Msg("git sync: comparing refs")

	// Same hash — no-op.
	if originHash == agentHash {
		s.mu.Unlock()
		return SyncResult{}, nil
	}

	originCommit, err := s.repo.CommitObject(originHash)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: origin commit: %w", err)
	}

	agentCommit, err := s.repo.CommitObject(agentHash)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: agent commit: %w", err)
	}

	// Check if origin is already an ancestor of agent (already merged).
	isOriginAncestor, err := originCommit.IsAncestor(agentCommit)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: check origin ancestor: %w", err)
	}
	if isOriginAncestor {
		s.mu.Unlock()
		log.Debug().Msg("git sync: origin already merged, nothing to do")
		return SyncResult{}, nil
	}

	// Check if agent HEAD is ancestor of origin → fast-forward.
	isAgentAncestor, err := agentCommit.IsAncestor(originCommit)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: check agent ancestor: %w", err)
	}
	if isAgentAncestor {
		newRef := plumbing.NewHashReference(agentRefName, originHash)
		if err := s.storer.SetReference(newRef); err != nil {
			s.mu.Unlock()
			return SyncResult{}, fmt.Errorf("Sync: fast-forward ref: %w", err)
		}
		s.mu.Unlock()

		log.Info().Str("to", originHash.String()[:8]).Msg("git sync: fast-forward")
		s.notifyCommit(originHash.String())
		if err := s.populateCommitLog(); err != nil {
			log.Warn().Err(err).Msg("commit_log: sync populate")
		}
		return SyncResult{Synced: true, FastForward: true}, nil
	}

	// Find merge base.
	bases, err := agentCommit.MergeBase(originCommit)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: merge base: %w", err)
	}
	if len(bases) == 0 {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: no common ancestor found (disjoint histories)")
	}
	baseCommit := bases[0]

	log.Debug().Str("base", baseCommit.Hash.String()[:8]).Msg("git sync: merge base")

	// Three-way merge: diff base→origin, apply to agent tree.
	mergedTreeHash, err := s.threeWayMerge(baseCommit, originCommit, agentCommit)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: three-way merge: %w", err)
	}

	// Create merge commit.
	mc := &object.Commit{
		Author:       s.authorSig("sync"),
		Committer:    s.committerSig(),
		Message:      fmt.Sprintf("sync: merge origin/%s into %s", remoteBranch, s.branch),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{agentHash, originHash},
	}

	commitObj := s.storer.NewEncodedObject()
	if err := mc.Encode(commitObj); err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: encode merge commit: %w", err)
	}
	mergeHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: store merge commit: %w", err)
	}

	if s.signer != nil {
		mergeHash, err = signCommitInPlace(s.storer, s.signer, mergeHash)
		if err != nil {
			s.mu.Unlock()
			return SyncResult{}, fmt.Errorf("Sync: sign merge commit: %w", err)
		}
	}

	newRef := plumbing.NewHashReference(agentRefName, mergeHash)
	if err := s.storer.SetReference(newRef); err != nil {
		s.mu.Unlock()
		return SyncResult{}, fmt.Errorf("Sync: update ref: %w", err)
	}

	s.mu.Unlock()

	log.Info().Str("merge_commit", mergeHash.String()[:8]).Msg("git sync: merged origin")
	s.notifyCommit(mergeHash.String())
	if err := s.populateCommitLog(); err != nil {
		log.Warn().Err(err).Msg("commit_log: sync populate")
	}
	return SyncResult{Synced: true, MergeCommit: mergeHash.String()}, nil
}

// threeWayMerge diffs base→origin and applies those changes to the agent tree.
// Origin wins for all changes (added, modified, deleted).
func (s *Store) threeWayMerge(baseCommit, originCommit, agentCommit *object.Commit) (plumbing.Hash, error) {
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("base tree: %w", err)
	}
	originTree, err := originCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("origin tree: %w", err)
	}
	agentTree, err := agentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("agent tree: %w", err)
	}

	changes, err := object.DiffTree(baseTree, originTree)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("diff tree: %w", err)
	}

	if len(changes) == 0 {
		return agentTree.Hash, nil
	}

	currentTree := agentTree

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("change action: %w", err)
		}

		switch action {
		case merkletrie.Insert, merkletrie.Modify:
			path := change.To.Name
			blobHash := change.To.TreeEntry.Hash

			if action == merkletrie.Modify {
				// Log if agent also modified this file relative to base.
				agentFile, agentErr := agentTree.File(path)
				baseFile, baseErr := baseTree.File(path)
				if agentErr == nil && baseErr == nil && agentFile.Hash != baseFile.Hash {
					log.Info().Str("path", path).Msg("sync: master overwrites agent change")
				}
			}

			newRootHash, err := buildTree(s.storer, currentTree, path, blobHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("apply %s %q: %w", action, path, err)
			}
			currentTree, err = object.GetTree(s.storer, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after %q: %w", path, err)
			}

		case merkletrie.Delete:
			path := change.From.Name

			// Log if agent modified the file that origin deleted.
			agentFile, agentErr := agentTree.File(path)
			baseFile, baseErr := baseTree.File(path)
			if agentErr == nil && baseErr == nil && agentFile.Hash != baseFile.Hash {
				log.Warn().Str("path", path).Msg("sync: master deletes agent-modified file")
			}

			newRootHash, err := deleteFromTree(s.storer, currentTree, path)
			if err != nil {
				// File might not exist in agent tree — skip.
				log.Debug().Str("path", path).Err(err).Msg("sync: skip delete (not in agent tree)")
				continue
			}
			currentTree, err = object.GetTree(s.storer, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after delete %q: %w", path, err)
			}
		}
	}

	return currentTree.Hash, nil
}

// ConfigureRemote ensures the origin remote is configured with the given URL
// and refspec for the specified branch.
func (s *Store) ConfigureRemote(url, branch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.repo.Config()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)

	if rc, ok := cfg.Remotes["origin"]; ok {
		if len(rc.URLs) > 0 && rc.URLs[0] == url {
			for _, rs := range rc.Fetch {
				if string(rs) == refspec {
					return nil // already configured
				}
			}
		}
	}

	// Delete existing origin if present, then create fresh.
	_ = s.repo.DeleteRemote("origin")
	_, err = s.repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
		Fetch: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(refspec),
		},
	})
	if err != nil {
		return fmt.Errorf("create remote: %w", err)
	}
	return nil
}

// PushResult is returned by Push to report what happened.
type PushResult struct {
	Pushed bool // true if refs were updated on remote
}

// Push pushes the agent branch to origin.
// Returns PushResult{Pushed: false} if already up to date.
//
// If the push fails with a non-fast-forward error, it retries with a force
// push. This is safe because agent branches are per-machine — no other machine
// writes to the same branch. Non-fast-forward errors typically happen after an
// origin session clone+swap reconstructs the agent branch from remote data.
func (s *Store) Push() (PushResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.repo.Remote("origin")
	if err != nil {
		log.Debug().Msg("git push: no origin remote configured, skipping")
		return PushResult{}, nil
	}

	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", s.branch, s.branch)

	log.Debug().Str("branch", s.branch).Msg("git push: pushing agent branch")
	err = s.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(refspec),
		},
		Auth: s.auth,
	})
	if err == gogit.NoErrAlreadyUpToDate {
		log.Debug().Msg("git push: already up to date")
		return PushResult{Pushed: false}, nil
	}
	if err != nil && strings.Contains(err.Error(), "non-fast-forward") {
		// Agent branches are per-machine, so force push is safe.
		log.Info().Str("branch", s.branch).Msg("git push: non-fast-forward, retrying with force push")
		forceRefspec := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", s.branch, s.branch)
		err = s.repo.Push(&gogit.PushOptions{
			RemoteName: "origin",
			RefSpecs: []gogitconfig.RefSpec{
				gogitconfig.RefSpec(forceRefspec),
			},
			Auth: s.auth,
		})
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("Push: %w", err)
	}

	log.Info().Str("branch", s.branch).Msg("git push: pushed agent branch")
	return PushResult{Pushed: true}, nil
}
