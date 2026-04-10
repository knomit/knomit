// Commit signature helpers and commit-notification glue on repoHandler.
// These were previously on factIndex but moved here because they are shared
// between factIndex writes and remoteIndex.Sync, and reaching SIDEWAYS through
// a sibling subsystem is not allowed.
package store

import (
	"context"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// authorSig returns the author signature for a given operation.
func (rh *repoHandler) authorSig(branch, operation string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "+" + operation + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// committerSig returns the committer signature (stable per agent).
func (rh *repoHandler) committerSig(branch string) object.Signature {
	agentID := deriveAgentID(branch)
	return object.Signature{
		Name:  agentID,
		Email: agentID + "@agents.knomit.io",
		When:  time.Now(),
	}
}

// notifyCommit appends the commit to commit_log and invokes the external
// observer (if registered). Called after every write that produces a new
// commit on a branch, outside the branch lock.
func (rh *repoHandler) notifyCommit(ctx context.Context, branch string, hash plumbing.Hash) {
	rh.AppendCommitLog(ctx, branch, hash.String())
	if rh.onCommit != nil {
		rh.onCommit(branch, hash.String())
	}
}
