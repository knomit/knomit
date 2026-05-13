// Remote synchronization: Sync orchestrates the reconcile primitives
// (reconcileMain + reconcileAgent) declared in remote_reconcile.go.
// Push force-pushes the agent branch — safe because only this machine
// writes its own agent branch, and Sync has already reconciled any
// upstream drift onto the local replayed history.
package store

import (
	"context"
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
)

// Sync runs one reconcile cycle for the agent branch:
//
//  1. Fetch origin (configured refspecs: main + agent/<host>).
//  2. Reconcile local main to origin/main (fast-forward or force-update on rewind).
//  3. Reconcile the agent branch against its upstream (origin/agent/<host>
//     if present, else origin/main).
//
// When reconcileMain reports Rewound, the agent still reconciles correctly
// because reconcileAgent reads the (new) local main via origin/main as
// fallback upstream when origin/agent/<host> isn't present. Main is
// reconciled FIRST so the agent sees the post-fetch tip.
//
// Safe to call repeatedly; each step is a no-op when there's nothing to do.
func (ri *remoteIndex) Sync(ctx context.Context, agentBranch string, auth transport.AuthMethod) (res SyncResult, retErr error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		log.Debug().Msg("Sync: no origin remote configured, skipping")
		return SyncResult{}, nil
	}

	// Past the "no remote" gate — write status on every return from here.
	defer func() {
		if retErr != nil {
			errMsg := retErr.Error()
			_ = ri.updateRemoteStatus("origin", "error", &errMsg)
		} else {
			_ = ri.updateRemoteStatus("origin", "ok", nil)
		}
	}()

	// Check if origin remote exists in git config.
	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("Sync: no origin git remote configured, skipping")
		return SyncResult{}, nil
	}

	// Fetch using the configured refspecs (Task 1 wrote two: main + agent).
	if err := ri.rh.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}

	return ri.reconcileNow(ctx, agentBranch)
}

// reconcileNow runs the post-fetch portion of Sync. Exposed (package-private)
// for tests that want to set up refs manually without a real remote.
//
// Acquires rh.lockBranch("main") for reconcileMain and releases it before
// reconcileAgent acquires rh.lockBranch(agentBranch). This avoids holding
// two branch locks simultaneously.
func (ri *remoteIndex) reconcileNow(ctx context.Context, agentBranch string) (SyncResult, error) {
	mainUnlock := ri.rh.lockBranch("main")
	mainRes, err := ri.rh.reconcileMain(ctx)
	mainUnlock()
	if err != nil {
		return SyncResult{Main: mainRes}, fmt.Errorf("Sync: reconcileMain: %w", err)
	}

	agentRes, err := ri.rh.reconcileAgent(ctx, agentBranch, StrategyLocalWins)
	if err != nil {
		return SyncResult{Main: mainRes, Agent: agentRes}, fmt.Errorf("Sync: reconcileAgent: %w", err)
	}

	return SyncResult{Main: mainRes, Agent: agentRes}, nil
}

// Push force-pushes the agent branch to origin. Force is safe because only
// this machine writes to its own agent branch; any upstream drift was
// reconciled by Sync (which Push callers should run first, and which the
// reconcile loop does run first per tick).
//
// Push does NOT push main — main is consensus, written by the remote-side
// merge-to-main mechanism, never directly by an agent.
//
// Returns Pushed=false (no error) when there is nothing to push (local
// agent ref already equals the last-known origin/agent ref).
func (ri *remoteIndex) Push(ctx context.Context, branch string, auth transport.AuthMethod) (res PushResult, retErr error) {
	unlock := ri.rh.lockBranch(branch)
	defer unlock()

	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("Push: no origin remote configured, skipping")
		return PushResult{}, nil
	}

	defer func() {
		if retErr != nil {
			errMsg := retErr.Error()
			_ = ri.updateRemotePushStatus("origin", "error", &errMsg)
		} else {
			_ = ri.updateRemotePushStatus("origin", "ok", nil)
		}
	}()

	// Already-up-to-date check: if local agent tip matches the last-known
	// origin/agent ref, nothing to push.
	localRef, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return PushResult{}, fmt.Errorf("Push: local ref: %w", err)
	}
	if remoteRef, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", branch)); err == nil {
		if remoteRef.Hash() == localRef.Hash() {
			return PushResult{Pushed: false}, nil
		}
	}

	// Force-push: local replayed history is the new truth on origin.
	refspec := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)
	if err := ri.rh.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
		Auth:       auth,
	}); err != nil {
		if err == gogit.NoErrAlreadyUpToDate {
			return PushResult{Pushed: false}, nil
		}
		return PushResult{}, fmt.Errorf("Push: %w", err)
	}

	log.Info().Str("branch", branch).Msg("Push: force-pushed")
	return PushResult{Pushed: true}, nil
}
