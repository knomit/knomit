// Remote synchronization: Sync orchestrates the reconcile primitives
// (reconcileMain + reconcileAgent) declared in remote_reconcile.go.
// Push force-pushes the agent branch — safe because only this machine
// writes its own agent branch, and Sync has already reconciled any
// upstream drift onto the local replayed history.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
)

// errNoOriginRemote signals (within Sync) that no "origin" git remote is
// configured, so the cycle is a no-op rather than a failure. Sentinel so the
// origin-existence check and the fetch can share one configMu read-lock scope.
var errNoOriginRemote = errors.New("no origin git remote configured")

// fetchOrigin fetches from origin using the configured refspecs. If origin
// does not have the agent branch yet (e.g. on first connect, before this
// machine has ever pushed), go-git returns NoMatchingRefSpecError. That is
// expected — fall back to fetching just refs/heads/<upstreamMain> so the
// consensus branch is populated and reconcile can run. The agent ref will
// materialize on the next fetch after the first Push.
//
// upstreamMain selects the consensus branch (typically "main", configurable
// to "master" or any other name). Empty defaults to "main".
func fetchOrigin(ctx context.Context, repo *gogit.Repository, auth transport.AuthMethod, upstreamMain string, timeout time.Duration) error {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	strictCtx, strictCancel := netCtxWith(ctx, timeout)
	err := repo.FetchContext(strictCtx, &gogit.FetchOptions{RemoteName: "origin", Auth: auth})
	strictCancel()
	if err == nil || errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return nil
	}
	var noMatch gogit.NoMatchingRefSpecError
	if !errors.As(err, &noMatch) {
		return err
	}
	// Strict refspec didn't match (almost certainly the agent ref). Retry
	// with just the upstream refspec — origin/<upstreamMain> is the only ref
	// we strictly require for reconcile to proceed.
	upstreamRefspec := gogitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", upstreamMain, upstreamMain))
	fallbackCtx, fallbackCancel := netCtxWith(ctx, timeout)
	fallbackErr := repo.FetchContext(fallbackCtx, &gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []gogitconfig.RefSpec{upstreamRefspec},
	})
	fallbackCancel()
	if fallbackErr == nil || errors.Is(fallbackErr, gogit.NoErrAlreadyUpToDate) {
		log.Debug().Str("upstream", upstreamMain).Msg("fetchOrigin: agent ref not on origin yet; fetched upstream only")
		return nil
	}
	// Surface both failures so a misconfigured refspec (which would make
	// the fallback fail with the same NoMatch error) is distinguishable
	// from a transport problem.
	return fmt.Errorf("fetchOrigin: strict fetch failed (%w); fallback fetch failed: %v", err, fallbackErr)
}

// Sync runs one reconcile cycle for the agent branch:
//
//  1. Fetch origin (configured refspecs: main + agent/<host>).
//  2. Reconcile local main to origin/main (fast-forward or force-update on rewind).
//  3. Reconcile the agent branch against local main, replaying any
//     local-only commits (since the watermark) onto the new main tip.
//     Main is reconciled FIRST so the agent sees the post-fetch tip.
//
// The agent's reconcile uses a per-branch watermark
// (refs/knomit/agent-base/<branch>) as the base for unpushedCommits, so
// main advances always propagate into the agent — even after the agent
// has previously pushed (the design bug this rework fixes). The rewind
// case is also handled by the watermark: when the watermark equals the
// old main and the agent has no local-only commits, the agent
// fast-forwards onto the new main and scrubbed files drop correctly.
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
			if statusErr := ri.updateRemoteStatus("origin", "error", &errMsg); statusErr != nil {
				log.Warn().Err(statusErr).Msg("Sync: failed to write error status to remotes table")
			}
		} else {
			if statusErr := ri.updateRemoteStatus("origin", "ok", nil); statusErr != nil {
				log.Warn().Err(statusErr).Msg("Sync: failed to write ok status to remotes table")
			}
		}
	}()

	upstreamMain := remote.Branch
	if upstreamMain == "" {
		upstreamMain = "main"
	}

	// Hold the config READ lock across the origin existence check and the fetch.
	// configureRemote (via Service.ConfigureRemote, which the origin HAL/session
	// handlers call whenever control.db's origin changes) rewrites the git
	// remote under configMu.Lock by DeleteRemote+CreateRemote; without this a
	// concurrent rewrite could delete origin out from under repo.Remote/Fetch
	// mid-cycle (a data race on go-git's config storer, and a transient
	// "no origin" / stale-refspec fetch). The lock is released before
	// reconcileNow, which works off the freshly-written remote-tracking refs.
	//
	// fetchOrigin tolerates a missing agent ref on origin (expected when the
	// agent branch has not yet been pushed) by falling back to fetching only
	// origin/<upstreamMain>.
	fetchErr := func() error {
		ri.rh.configMu.RLock()
		defer ri.rh.configMu.RUnlock()
		if _, err := ri.rh.repo.Remote("origin"); err != nil {
			return errNoOriginRemote
		}
		return fetchOrigin(ctx, ri.rh.repo, auth, upstreamMain, ri.rh.netTimeout)
	}()
	if fetchErr == errNoOriginRemote {
		log.Debug().Msg("Sync: no origin git remote configured, skipping")
		return SyncResult{}, nil
	}
	if fetchErr != nil {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", fetchErr)
	}

	return ri.reconcileNow(ctx, agentBranch, upstreamMain)
}

// reconcileNow runs the post-fetch portion of Sync. Exposed (package-private)
// for tests that want to set up refs manually without a real remote.
//
// Acquires rh.lockBranch(upstreamMain) for reconcileMain and releases it
// before reconcileAgent acquires rh.lockBranch(agentBranch). This avoids
// holding two branch locks simultaneously.
//
// upstreamMain is the consensus branch name (typically "main" but
// configurable to "master" or any other). Empty defaults to "main".
func (ri *remoteIndex) reconcileNow(ctx context.Context, agentBranch, upstreamMain string) (SyncResult, error) {
	if upstreamMain == "" {
		upstreamMain = "main"
	}

	// Degenerate config: the configured consensus branch IS this machine's
	// own agent branch (e.g. a clone whose remote HEAD was an agent branch,
	// written by a pre-#82 InitFromRemote that adopted the remote agent-branch
	// HEAD as upstream). Reconciling the agent branch against itself as "main"
	// makes reconcileMain force-reset the local branch down to origin whenever
	// it is ahead — destroying just-written, not-yet-pushed fact commits.
	// There is nothing to pull from a consensus branch that does not exist
	// independently of the agent, so skip all reconcile and let the caller's
	// Push carry local commits up: push-only.
	if upstreamMain == agentBranch {
		log.Warn().
			Str("branch", agentBranch).
			Msg("reconcileNow: upstream main equals agent branch; skipping pull/reconcile (push-only). Set a real consensus branch (e.g. main) to re-enable pulls.")
		return SyncResult{}, nil
	}

	mainRes, err := func() (MainReconcileResult, error) {
		defer ri.rh.lockBranch(upstreamMain)()
		return ri.rh.reconcileMain(ctx, upstreamMain)
	}()
	if err != nil {
		return SyncResult{Main: mainRes}, fmt.Errorf("Sync: reconcileMain: %w", err)
	}

	// reconcileAgent dispatches on mainRes.Mode:
	//   - !ModeRewound → merge local upstream into agent (steady state, one merge commit at most).
	//   - ModeRewound  → rebase fallback: replay agent's local-only commits onto the
	//                    disjoint new upstream, dropping any files scrubbed by the rewind.
	agentRes, err := ri.rh.reconcileAgent(ctx, agentBranch, upstreamMain, StrategyLocalWins, mainRes.Mode == ModeRewound)
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
			if statusErr := ri.updateRemotePushStatus("origin", "error", &errMsg); statusErr != nil {
				log.Warn().Err(statusErr).Msg("Push: failed to write error status to remotes table")
			}
		} else {
			if statusErr := ri.updateRemotePushStatus("origin", "ok", nil); statusErr != nil {
				log.Warn().Err(statusErr).Msg("Push: failed to write ok status to remotes table")
			}
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
	pushCtx, pushCancel := ri.rh.netCtx(ctx)
	defer pushCancel()
	if err := ri.rh.repo.PushContext(pushCtx, &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
		Auth:       auth,
	}); err != nil {
		if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return PushResult{Pushed: false}, nil
		}
		return PushResult{}, fmt.Errorf("Push: %w", err)
	}

	log.Info().Str("branch", branch).Msg("Push: force-pushed")
	return PushResult{Pushed: true}, nil
}
