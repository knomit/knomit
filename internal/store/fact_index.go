package store

import (
	"context"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
)

func parseFact(path, content string) (FactRecord, error) {
	f, err := fact.ParseFact(path, content)
	if err != nil {
		return FactRecord{}, err
	}
	return FactRecord{
		Path:           f.Path(),
		Title:          f.Title,
		Type:           string(f.Type),
		Domain:         f.Domain,
		Entities:       f.Entities,
		Confidence:     f.Confidence,
		Sources:        f.Sources,
		Refs:           f.Refs,
		EvidenceWeight: f.EvidenceWeight,
	}, nil
}

// Compile-time interface checks.
var _ FactIndex = (*factIndex)(nil)

// factIndex owns all git-backed fact operations: reading, writing, and commit-log
// management. It is embedded in Service so that Service satisfies FactIndex and
// gitReader without code duplication.
type factIndex struct {
	rh         *repoHandler
	branchMu   sync.Map // per-branch write serialization
	auth       transport.AuthMethod
	signer     ssh.Signer
	onCommit   func(branch, hash string)
	postCommit func(ctx context.Context, branch string) error // wired to si.Sync
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
	return fi.rh.HeadCommit(ctx, branch)
}
