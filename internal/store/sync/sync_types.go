package sync

import (
	"errors"

	"knomit/internal/store/merge"
)

// SyncResult is the bundled outcome of a sync tick — one reconcile cycle
// that brings local main to origin/main (Main) and reconciles the agent
// branch (Agent) via merge or rebase fallback.
type SyncResult struct {
	Main  MainReconcileResult        `json:"main"`
	Agent merge.AgentReconcileResult `json:"agent"`
}

// PushResult is returned by Push to report what happened.
type PushResult struct {
	Pushed bool // true if refs were updated on remote
}

// ErrNoAgentBranch is returned by Push when asked to push an empty branch
// name: a subscription has no agent branch and nothing of its own to push.
var ErrNoAgentBranch = errors.New("repo has no agent branch to push")
