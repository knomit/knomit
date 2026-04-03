package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/store/git"
)

// factIndex owns all git-backed fact operations: reading, writing, and commit-log
// management. It is embedded in Service so that Service satisfies FactIndex and
// gitReader without code duplication.
type factIndex struct {
	rh         *repoHandler
	gits       *storegit.Storer
	repo       *gogit.Repository // nil until OpenRepo/InitRepo/Clone called
	branchMu   sync.Map          // per-branch write serialization
	configMu   sync.Mutex        // guards ConfigureRemote
	auth       transport.AuthMethod
	signer     ssh.Signer
	onCommit   func(branch, hash string)
	postCommit func(ctx context.Context, git gitReader, branch string) error // wired to si.Sync in Step 6
}

// Compile-time interface checks.
var _ gitReader = (*factIndex)(nil)
var _ FactIndex = (*factIndex)(nil)

// resolveRef returns the commit hash at the tip of branch.
func (fi *factIndex) resolveRef(ctx context.Context, branch string) (plumbing.Hash, error) {
	ref, err := fi.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("resolveRef %q: %w", branch, err)
	}
	return ref.Hash(), nil
}

// lockBranch acquires the per-branch mutex and returns an unlock function.
func (fi *factIndex) lockBranch(branch string) func() {
	v, _ := fi.branchMu.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// authorSig returns the author signature for a given operation.
func (fi *factIndex) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (fi *factIndex) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// notifyCommit calls appendCommitLog and then the optional external callback.
func (fi *factIndex) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) {
	fi.appendCommitLog(ctx, branch, hash)
	if fi.onCommit != nil {
		fi.onCommit(branch, hash.String())
	}
}

// HeadCommit returns the hash of the tip commit of branch as a hex string.
func (fi *factIndex) HeadCommit(ctx context.Context, branch string) (string, error) {
	hash, err := fi.resolveRef(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("HeadCommit: %w", err)
	}
	return hash.String(), nil
}

// createBranch creates a new branch ref pointing at the tip of fromBranch.
// No-op if branch already exists.
func (fi *factIndex) createBranch(ctx context.Context, branch, fromBranch string) error {
	newRefName := plumbing.NewBranchReferenceName(branch)
	if _, err := fi.gits.Reference(newRefName); err == nil {
		return nil // already exists
	}
	fromHash, err := fi.resolveRef(ctx, fromBranch)
	if err != nil {
		return fmt.Errorf("createBranch: resolve source %q: %w", fromBranch, err)
	}
	if err := fi.gits.SetReference(plumbing.NewHashReference(newRefName, fromHash)); err != nil {
		return fmt.Errorf("createBranch: set ref: %w", err)
	}
	log.Info().Str("branch", branch).Str("from", fromBranch).Msg("created branch")
	return nil
}
