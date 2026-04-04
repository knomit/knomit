package store

import (
	"context"
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
	return NewFactRecord(f, ""), nil
}

// Compile-time interface checks.
var _ FactIndex = (*factIndex)(nil)

// factIndex owns all git-backed fact operations: reading and writing.
type factIndex struct {
	rh         *repoHandler
	auth       transport.AuthMethod
	signer     ssh.Signer
	onCommit   func(branch, hash string)
	postCommit func(ctx context.Context, branch string) error // wired to si.Sync
	appendLog  func(ctx context.Context, branch, hash string) // wired to si.appendCommitLog
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

// notifyCommit appends to commit_log (via callback) and invokes the external observer.
func (fi *factIndex) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) {
	if fi.appendLog != nil {
		fi.appendLog(ctx, branch, hash.String())
	}
	if fi.onCommit != nil {
		fi.onCommit(branch, hash.String())
	}
}
