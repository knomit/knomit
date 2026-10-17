// Remote synchronization: Sync orchestrates the reconcile primitives
// (reconcileMain + reconcileAgent) declared in remote_reconcile.go.
// Push force-pushes the agent branch — safe because only this machine
// writes its own agent branch, and Sync has already reconciled any
// upstream drift onto the local replayed history.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
// upstreamMain is the consensus branch, never empty here: every caller has
// resolved it (InitFromRemote, InitSubscription) or refused an empty one (Sync).
//
// extra refspecs are fetched beside the configured ones, in both attempts,
// without being written to the git config: the fleet master's peerRefspec,
// decided per round (SyncWithPeers). A wildcard refspec never fails to match,
// so it cannot cause the fallback.
//
// With extra, the strict attempt also PRUNES (PR #524 N1): a tracking ref
// under a refspec's destination whose branch the origin no longer has is
// removed, so a member branch deleted on the forge is forgotten by the
// master. The prune stays within the extra wildcard: the strict attempt
// succeeds only when every exact configured refspec matched (go-git's
// calculateRefs runs before the prune and fails with NoMatchingRefSpecError
// otherwise), so those refs exist on the origin and are never pruned. The
// fallback attempt (the own branch absent on the origin) does not prune.
func fetchOrigin(ctx context.Context, repo *gogit.Repository, auth transport.AuthMethod, upstreamMain string, timeout time.Duration, extra ...gogitconfig.RefSpec) error {
	opts := &gogit.FetchOptions{RemoteName: "origin", Auth: auth}
	if len(extra) > 0 {
		rem, err := repo.Remote("origin")
		if err != nil {
			return err
		}
		opts.RefSpecs = append(append([]gogitconfig.RefSpec(nil), rem.Config().Fetch...), extra...)
		opts.Prune = true
	}
	strictCtx, strictCancel := netCtxWith(ctx, timeout)
	err := repo.FetchContext(strictCtx, opts)
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
		RefSpecs:   append([]gogitconfig.RefSpec{upstreamRefspec}, extra...),
	})
	fallbackCancel()
	if fallbackErr == nil || errors.Is(fallbackErr, gogit.NoErrAlreadyUpToDate) {
		log.Debug().Str("upstream", upstreamMain).Msg("fetchOrigin: agent ref not on origin yet; fetched upstream only")
		// The origin has no branch for the other (non-wildcard) configured
		// refspecs: their remote-tracking refs mirror that. A tracking ref left
		// from earlier (another origin URL, or a branch deleted on the forge)
		// would read in Push as "the origin already has this" and nothing
		// would be sent.
		if rem, rerr := repo.Remote("origin"); rerr == nil {
			for _, rs := range rem.Config().Fetch {
				if rs.IsWildcard() || rs == upstreamRefspec {
					continue
				}
				dst := rs.Dst(plumbing.ReferenceName(rs.Src()))
				if err := repo.Storer.RemoveReference(dst); err != nil {
					log.Warn().Err(err).Str("ref", dst.String()).Msg("fetchOrigin: could not drop a tracking ref the origin has no branch for")
				}
			}
		}
		return nil
	}
	// Surface both failures so a misconfigured refspec (which would make
	// the fallback fail with the same NoMatch error) is distinguishable
	// from a transport problem.
	return fmt.Errorf("fetchOrigin: strict fetch failed (%w); fallback fetch failed: %v", err, fallbackErr)
}

// abandonedByCaller reports whether this attempt ended because OUR context was
// CANCELLED, rather than because the remote said something.
//
// Such an attempt established NOTHING about the remote, so it must not
// overwrite what the last completed one established. The reported case: a
// create against a remote finishes by calling ActivateSync, which cancels the
// reconcile loop to restart it (repos/builder.go). A tick whose fetch was in
// flight died with "context canceled", and that was written here as the
// remote's status — so a create that fully succeeded, agent branch pushed and
// all, showed "sync failed — Sync: fetch: ...: context canceled" on the repo
// screen. The repository was fine; the message was about knomit stopping its
// own request.
//
// CANCELLATION ONLY — a DEADLINE is a real failure and is still recorded.
// The two are not interchangeable just because both land in ctx.Err():
// recoverFromOrigin gives the startup reconcile a 15s budget (builder.go), and
// a remote that does not answer inside it has genuinely failed. Treating every
// non-nil ctx.Err() as ours would leave a stale "ok" on a remote that was
// unreachable at boot, which is worse than the message this function exists to
// suppress: a false failure is noise, a false success is a lie.
//
// Both halves are required. `ctx.Err()` being Cancelled says we stopped; the
// error ITSELF wrapping context.Canceled says that is why this attempt ended.
// Without the second half the test is a coincidence rather than a cause, and a
// real refusal that lands microseconds before a cancellation is discarded.
func abandonedByCaller(ctx context.Context, retErr error) bool {
	return retErr != nil &&
		errors.Is(ctx.Err(), context.Canceled) &&
		errors.Is(retErr, context.Canceled)
}

// Sync runs one reconcile cycle for the agent branch:
//
//  1. Fetch origin (configured refspecs: main + agent/<host>).
//  2. Reconcile local main to origin/main (fast-forward or force-update on rewind).
//  3. Reconcile the agent branch against local main, replaying any
//     local-only commits (since the watermark) onto the new main tip.
//     Main is reconciled FIRST so the agent sees the post-fetch tip.
//     When agentBranch is empty (a subscription) step 3 is skipped.
//
// The agent's reconcile uses a per-branch watermark
// (refs/knomit/agent-base/<branch>) as the base for unpushedCommits, so
// main advances always propagate into the agent — even after the agent
// has previously pushed (the design bug this rework fixes). The rewind
// case is also handled by the watermark: when the watermark equals the
// old main and the agent has no local-only commits, the agent branch is
// rebuilt on the new main (ModeRebase, nothing replayed; see
// replayOntoUpstream) and scrubbed files drop correctly.
//
// Safe to call repeatedly; each step is a no-op when there's nothing to do.
func (ri *remoteIndex) Sync(ctx context.Context, agentBranch string, auth transport.AuthMethod) (SyncResult, error) {
	return ri.sync(ctx, agentBranch, auth)
}

// peerRefspec is the fleet master's third fetch refspec: every member's agent
// branch, into remote-tracking refs (refs/remotes/origin/agent/*) that
// nothing reads as a branch. Only AdmitPeerBranch moves a local branch to one
// of their tips, after judging it.
const peerRefspec = gogitconfig.RefSpec("+refs/heads/agent/*:refs/remotes/origin/agent/*")

// SyncWithPeers is Sync for the fleet's master when its copy of the fleet
// repository has an origin (a forge): the same round, with every agent/*
// branch fetched as well (peerRefspec). The refspec rides THIS fetch only and
// is never written to the git config, so a member's remote never has it, and
// an instance that gains or loses the fleet root key changes what it fetches
// on its next round, without a restart.
func (ri *remoteIndex) SyncWithPeers(ctx context.Context, agentBranch string, auth transport.AuthMethod) (SyncResult, error) {
	return ri.sync(ctx, agentBranch, auth, peerRefspec)
}

func (ri *remoteIndex) sync(ctx context.Context, agentBranch string, auth transport.AuthMethod, extra ...gogitconfig.RefSpec) (res SyncResult, retErr error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		log.Debug().Msg("Sync: no origin remote configured, skipping")
		return SyncResult{}, nil
	}

	// Past the "no remote" gate — write status on every return from here, with
	// one exception: an attempt WE stopped (see abandonedByCaller).
	defer func() {
		if abandonedByCaller(ctx, retErr) {
			log.Debug().Err(retErr).Msg("Sync: cancelled by caller; leaving the last established status")
			return
		}
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

	// The origin's branch is the consensus branch. An origin without one names
	// nothing to reconcile against, and is reported rather than guessed at: this
	// is the one entry that reads it, so fetchOrigin, reconcileNow and the
	// reconcile steps below never see an empty name.
	upstreamMain := remote.Branch
	if upstreamMain == "" {
		return SyncResult{}, fmt.Errorf("Sync: the origin has no branch: %w", ErrNoConsensusBranch)
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
	//
	// lastOrigin is where origin/<upstreamMain> pointed BEFORE this fetch
	// moves it (zero when there was no such ref): the fallback watermark of a
	// branch that has none (seedMissingWatermark). It can only be read here.
	var lastOrigin plumbing.Hash
	fetchErr := func() error {
		ri.rh.configMu.RLock()
		defer ri.rh.configMu.RUnlock()
		if _, err := ri.rh.repo.Remote("origin"); err != nil {
			return errNoOriginRemote
		}
		if ref, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", upstreamMain)); err == nil {
			lastOrigin = ref.Hash()
		}
		return fetchOrigin(ctx, ri.rh.repo, auth, upstreamMain, ri.rh.netTimeout, extra...)
	}()
	if fetchErr == errNoOriginRemote {
		log.Debug().Msg("Sync: no origin git remote configured, skipping")
		return SyncResult{}, nil
	}
	if fetchErr != nil {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", fetchErr)
	}
	if h := currentStoreHooks().afterFetch; h != nil {
		if err := h(); err != nil {
			return SyncResult{}, fmt.Errorf("Sync: %w", err)
		}
	}

	// The origin snapshot (origin_snapshot.go): the last origin state this
	// copy fully reconciled against. When it belongs to another origin than
	// this one, the agent is reconciled by the cross-origin classification;
	// otherwise by the existing rules. It is replaced only at the end of this
	// round, after the reconcile succeeded, with what was fetched now.
	snap, err := ri.rh.readOriginSnapshot()
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: %w", err)
	}
	identity := originIdentity(remote.URL)
	var fetched plumbing.Hash
	if ref, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", upstreamMain)); err == nil {
		fetched = ref.Hash()
	}
	var across originSnapshot
	switch {
	case snap.ok() && snap.Identity != identity:
		across = snap
	case snap.ok() && fetched != plumbing.ZeroHash && !snap.has(fetched):
		// The same origin: cover what this round may take in BEFORE the
		// reconcile moves the branch onto it. Added to the set, never
		// written alone (origin_snapshot.go).
		snap.Commits = append(snap.Commits, fetched)
		if err := ri.rh.writeOriginSnapshot(snap); err != nil {
			return SyncResult{}, fmt.Errorf("Sync: %w", err)
		}
	}

	if agentBranch != "" && agentBranch != upstreamMain {
		if err := ri.rh.seedMissingWatermark(agentBranch, lastOrigin); err != nil {
			return SyncResult{}, fmt.Errorf("Sync: %w", err)
		}
	}

	res, err = ri.reconcileNowAcross(ctx, agentBranch, upstreamMain, across)
	if err != nil {
		return res, err
	}
	if h := currentStoreHooks().beforeOriginSnapshot; h != nil {
		if err := h(); err != nil {
			return res, fmt.Errorf("Sync: %w", err)
		}
	}
	if fetched != plumbing.ZeroHash {
		if err := ri.rh.writeOriginSnapshot(originSnapshot{Identity: identity, Commits: []plumbing.Hash{fetched}}); err != nil {
			return res, fmt.Errorf("Sync: %w", err)
		}
	}
	return res, nil
}

// reconcileNow runs the post-fetch portion of Sync. Exposed (package-private)
// for tests that want to set up refs manually without a real remote.
//
// Acquires rh.lockBranch(upstreamMain) for reconcileMain and releases it
// before reconcileAgent acquires rh.lockBranch(agentBranch). This avoids
// holding two branch locks simultaneously.
//
// upstreamMain is the consensus branch name; Sync, the only production
// caller, refuses an empty one before getting here.
func (ri *remoteIndex) reconcileNow(ctx context.Context, agentBranch, upstreamMain string) (SyncResult, error) {
	return ri.reconcileNowAcross(ctx, agentBranch, upstreamMain, originSnapshot{})
}

// reconcileNowAcross is reconcileNow with the origin snapshot of ANOTHER
// origin (zero when the snapshot is this origin's, or there is none): the
// agent is then reconciled by reconcileAgentAcrossOrigins, a function of the
// branch, main and that snapshot alone, whatever the main reconcile did.
func (ri *remoteIndex) reconcileNowAcross(ctx context.Context, agentBranch, upstreamMain string, across originSnapshot) (SyncResult, error) {

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

	// Stated assumption (user ruling 2026-10-15, PR #524 S1): the origin's main is trusted as fetched, rewinds included, on the fleet master too; branch protection on it is the forge admin's job.
	mainRes, err := func() (MainReconcileResult, error) {
		defer ri.rh.lockBranch(upstreamMain)()
		return ri.rh.reconcileMain(ctx, upstreamMain)
	}()
	if err != nil {
		return SyncResult{Main: mainRes}, fmt.Errorf("Sync: reconcileMain: %w", err)
	}

	// A subscription has no agent branch: the upstream IS what this repo reads,
	// so the main reconcile is the whole sync. There is no local-only work to
	// replay and nothing to merge into.
	if agentBranch == "" {
		return SyncResult{Main: mainRes}, nil
	}

	// reconcileAgent dispatches on mainRes.Mode:
	//   - !ModeRewound → merge local upstream into agent (steady state, one merge commit at most).
	//   - ModeRewound  → rebase fallback: replay agent's local-only commits onto the
	//                    disjoint new upstream, dropping any files scrubbed by the rewind.
	//
	// The strategy is the repo's `conflicts` setting, read at the tip of the
	// consensus branch just reconciled to origin's (the host reads the same
	// value at its own): set → each key as it says, the incoming consensus
	// branch (src) being the consensus side and a key set to off still going
	// LocalWins; both keys off → LocalWins, as always.
	strategy := StrategyLocalWins
	if s, on := ri.rh.conflictsStrategy(ctx, upstreamMain); on {
		strategy = s
	}
	if h := currentStoreHooks().afterMainReconcile; h != nil {
		if err := h(string(mainRes.Mode)); err != nil {
			return SyncResult{Main: mainRes}, fmt.Errorf("Sync: %w", err)
		}
	}
	var agentRes AgentReconcileResult
	if across.ok() {
		agentRes, err = ri.rh.reconcileAgentAcrossOrigins(ctx, agentBranch, upstreamMain, strategy, across)
	} else {
		agentRes, err = ri.rh.reconcileAgent(ctx, agentBranch, upstreamMain, strategy, mainRes.Mode == ModeRewound)
	}
	if err != nil {
		return SyncResult{Main: mainRes, Agent: agentRes}, fmt.Errorf("Sync: reconcileAgent: %w", err)
	}

	// The left-out report is derived from the durable left-out refs every
	// round, whichever path ran (origin_snapshot.go).
	if n, origins, refs, rerr := ri.rh.leftOutReport(agentBranch, upstreamMain); rerr != nil {
		log.Warn().Err(rerr).Str("branch", agentBranch).Msg("Sync: could not derive the left-out report")
	} else {
		agentRes.LeftOut, agentRes.LeftOutOrigins, agentRes.LeftOutRefs = n, origins, refs
	}
	return SyncResult{Main: mainRes, Agent: agentRes}, nil
}

// Push force-pushes the agent branch to origin. Force is safe because only
// this machine writes to its own agent branch; any upstream drift was
// reconciled by Sync (which Push callers should run first, and which the
// reconcile loop does run first per tick).
//
// Push does NOT push main — main is consensus, written by the remote-side
// merge-to-main mechanism, never directly by an agent. The one exception is
// the fleet master's write of the fleet repository's main, PublishUpstream.
//
// Returns Pushed=false (no error) when there is nothing to push (the snapshot
// already equals the last-known origin/agent ref).
//
// A SNAPSHOT push (F21 S1; user, 2026-10-01: "when a push triggers, fetch the
// current commit, then push up until that commit"). The branch's write lock is
// held only to read its tip S, then released, and S is pushed BY HASH
// (+<S>:refs/heads/<branch>). So writes to the branch never wait on the
// network, and nothing written after the snapshot changes what this push
// sends: it goes out on the next push. Before this, the write lock was held
// through PushContext, and a hanging origin stalled every write for up to
// network_timeout.
//
// pushMu (per branch) still serializes pushes from this instance — the sync
// loop, the fleet operations' one push and the create-time push — so one
// push's snapshot is always taken after the previous push finished. It is NOT
// a guarantee about what the origin applies: a push that hits network_timeout
// releases pushMu while the server may still apply it. Against a knomit host
// the guard for that case is the host's compare-and-set on the advertised old
// value (httphandler_receivepack.go, "moved since it was advertised").
//
// After a successful push go-git sets refs/remotes/origin/<branch> to the hash
// it pushed (S, the command's New) through the configured fetch spec, so the
// tracking ref records exactly what the origin now has.
//
// An empty branch is refused with ErrNoAgentBranch, but that is a BACKSTOP:
// the reconcile loop gates on repos.pushAllowed and never calls this for a
// subscription in the first place.
func (ri *remoteIndex) Push(ctx context.Context, branch string, auth transport.AuthMethod) (res PushResult, retErr error) {
	if branch == "" {
		return PushResult{}, ErrNoAgentBranch
	}

	unlockPush := ri.rh.lockPush(branch)
	defer unlockPush()

	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("Push: no origin remote configured, skipping")
		return PushResult{}, nil
	}
	if h := currentStoreHooks().beforePush; h != nil {
		if err := h(branch); err != nil {
			return PushResult{}, fmt.Errorf("Push: %w", err)
		}
	}

	defer func() {
		if abandonedByCaller(ctx, retErr) {
			log.Debug().Err(retErr).Msg("Push: cancelled by caller; leaving the last established status")
			return
		}
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

	// The snapshot: the branch's tip, read under the write lock and nothing
	// else. The lock is released before any network I/O.
	snap, err := func() (plumbing.Hash, error) {
		defer ri.rh.lockBranch(branch)()
		localRef, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return localRef.Hash(), nil
	}()
	if err != nil {
		return PushResult{}, fmt.Errorf("Push: local ref: %w", err)
	}

	// Already-up-to-date check: the snapshot matches the last-known
	// origin/agent ref, so there is nothing to push.
	if remoteRef, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", branch)); err == nil {
		if remoteRef.Hash() == snap {
			return PushResult{Pushed: false}, nil
		}
	}

	// go-git resolves a hash source with object.GetCommit, and when that fails
	// it adds NO command and returns NoErrAlreadyUpToDate (go-git v5.19.2
	// remote.go, addOrUpdateReferences): a push that sent nothing would read
	// as "up to date". So the snapshot's commit must resolve here, and an
	// unreadable one is an error, never a silent Pushed:false.
	if _, err := ri.rh.repo.CommitObject(snap); err != nil {
		return PushResult{}, fmt.Errorf("Push: snapshot commit %s is unreadable: %w", snap, err)
	}

	if h := currentStoreHooks().afterSnapshot; h != nil {
		h(branch, snap)
	}

	// Force-push the snapshot BY HASH: local replayed history is the new truth
	// on origin, and a commit made after the snapshot is not in this push.
	refspec := fmt.Sprintf("+%s:refs/heads/%s", snap, branch)
	pushCtx, pushCancel := ri.rh.netCtx(ctx)
	defer pushCancel()
	// Progress is the SERVER'S SIDE of the conversation, and without it a
	// refusal arrives as go-git's summary alone: "command error on
	// refs/heads/main: pre-receive hook declined". The reason a hook declined
	// — protected branch, push rule, unsigned commit, oversized file — is sent
	// on the sideband as `remote:` lines, and dropping them left a reader
	// staring at the fact of a refusal with no way to learn its cause.
	var srv bytes.Buffer
	if err := ri.rh.repo.PushContext(pushCtx, &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
		Auth:       auth,
		Progress:   &srv,
	}); err != nil {
		if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return PushResult{Pushed: false}, nil
		}
		return PushResult{}, fmt.Errorf("Push: %w%s", err, remoteSays(srv.String()))
	}

	log.Info().Str("branch", branch).Str("commit", snap.String()).Msg("Push: force-pushed")
	return PushResult{Pushed: true}, nil
}

// PublishUpstream pushes the tip of the local branch from to the origin's
// consensus branch (the origin's Branch, "main"), by hash and WITHOUT force
// (<tip>:refs/heads/<main>, no '+'). It is the one push that writes a
// consensus branch, and only the fleet master calls it, for the fleet
// repository (Push above never writes main). Without force the origin refuses
// an update that is not a fast-forward, so a main the caller has not seen is
// never overwritten: that refusal is an ordinary push error.
//
// It holds the push lock of the consensus branch, reads from's tip under
// from's write lock, and records nothing in the remotes table: the caller
// reports the outcome. Pushed is false when the origin's main already is the
// tip.
func (ri *remoteIndex) PublishUpstream(ctx context.Context, from string, auth transport.AuthMethod) (PushResult, error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		return PushResult{}, fmt.Errorf("PublishUpstream: no origin: %v", err)
	}
	upstream := remote.Branch
	if upstream == "" {
		return PushResult{}, fmt.Errorf("PublishUpstream: the origin has no branch: %w", ErrNoConsensusBranch)
	}
	if from == "" || from == upstream {
		return PushResult{}, fmt.Errorf("PublishUpstream: %q is not a branch to publish to %q", from, upstream)
	}
	defer ri.rh.lockPush(upstream)()
	tip, err := func() (plumbing.Hash, error) {
		defer ri.rh.lockBranch(from)()
		ref, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(from))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return ref.Hash(), nil
	}()
	if err != nil {
		return PushResult{}, fmt.Errorf("PublishUpstream: local ref %s: %w", from, err)
	}
	if _, err := ri.rh.repo.CommitObject(tip); err != nil {
		return PushResult{}, fmt.Errorf("PublishUpstream: tip %s is unreadable: %w", tip, err)
	}
	// The commits the publish adds to the origin's main are judged first
	// (judgeOwnRange): tip ∖ the main this copy last fetched from the origin
	// (its local main when it has no tracking ref yet).
	if err := ri.rh.judgeOwnRange(upstream, ri.publishBase(upstream), tip, ri.ownID(from), tip); err != nil {
		return PushResult{}, fmt.Errorf("PublishUpstream: %w", err)
	}
	refspec := fmt.Sprintf("%s:refs/heads/%s", tip, upstream)
	pushCtx, cancel := ri.rh.netCtx(ctx)
	defer cancel()
	var srv bytes.Buffer
	if err := ri.rh.repo.PushContext(pushCtx, &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
		Auth:       auth,
		Progress:   &srv,
	}); err != nil {
		if errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return PushResult{Pushed: false}, nil
		}
		return PushResult{}, fmt.Errorf("PublishUpstream: %w%s", err, remoteSays(srv.String()))
	}
	log.Info().Str("upstream", upstream).Str("from", from).Str("commit", tip.String()).Msg("PublishUpstream: pushed")
	return PushResult{Pushed: true}, nil
}

// PublishTo pushes the tip of the local branch from to gitURL as BOTH
// refs/heads/<upstream> (without force, so a main there that the tip does not
// descend from is refused by the remote) and refs/heads/<from> (with force:
// the branch is this instance's own), in one push. gitURL need not be the
// origin, and nothing is configured or recorded: the push goes through a
// remote that exists for this call only, so the git config, the
// remote-tracking refs and the remotes table are untouched.
//
// It is the fleet master's seeding of a forge BEFORE the forge becomes the
// origin (repos.Manager.moveFleetToForge): the origin round that the attach
// starts then fetches a main equal to the tip, and never rewinds local main
// to an older forge main (kb/decisions/fleet/trust-the-forge-main).
func (ri *remoteIndex) PublishTo(ctx context.Context, gitURL, from, upstream string, auth transport.AuthMethod) error {
	if from == "" || upstream == "" || from == upstream {
		return fmt.Errorf("PublishTo: %q is not a branch to publish to %q", from, upstream)
	}
	defer ri.rh.lockPush(upstream)()
	defer ri.rh.lockPush(from)()
	tip, err := func() (plumbing.Hash, error) {
		defer ri.rh.lockBranch(from)()
		ref, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(from))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return ref.Hash(), nil
	}()
	if err != nil {
		return fmt.Errorf("PublishTo: local ref %s: %w", from, err)
	}
	if _, err := ri.rh.repo.CommitObject(tip); err != nil {
		return fmt.Errorf("PublishTo: tip %s is unreadable: %w", tip, err)
	}
	// The seed becomes the forge's main: judged first (judgeOwnRange), tip ∖
	// this copy's own main.
	base := plumbing.ZeroHash
	if ref, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(upstream)); err == nil {
		base = ref.Hash()
	}
	if err := ri.rh.judgeOwnRange(upstream, base, tip, ri.ownID(from), tip); err != nil {
		return fmt.Errorf("PublishTo: %w", err)
	}
	rem := gogit.NewRemote(ri.rh.repo.Storer, &gogitconfig.RemoteConfig{Name: "publish-to", URLs: []string{gitURL}})
	pushCtx, cancel := ri.rh.netCtx(ctx)
	defer cancel()
	var srv bytes.Buffer
	err = rem.PushContext(pushCtx, &gogit.PushOptions{
		RemoteName: "publish-to",
		RefSpecs: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(fmt.Sprintf("%s:refs/heads/%s", tip, upstream)),
			gogitconfig.RefSpec(fmt.Sprintf("+%s:refs/heads/%s", tip, from)),
		},
		Auth:     auth,
		Progress: &srv,
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("PublishTo: %w%s", err, remoteSays(srv.String()))
	}
	log.Info().Str("upstream", upstream).Str("from", from).Str("commit", tip.String()).Msg("PublishTo: pushed")
	return nil
}

// RetractPushedBranch takes back what this instance pushed of branch, for a
// repository that is about to be deleted (a fleet create that is being rolled
// back). It deletes refs/heads/<branch> on the origin WITH A LEASE on the
// last tip this instance saw there, pushed OR fetched: the tracking ref
// refs/remotes/origin/<branch>, which every successful Push sets and every
// fetch of the branch's refspec updates. Then it
// removes the origin from this repository's git config, so that no later push
// recreates the branch (the reconcile loop's Push skips a repository without
// an origin remote). Both steps run under the branch's push lock, which every
// Push takes, so no push can land between them.
//
// The lease is the origin's compare-and-swap. The delete command carries the
// old value from the push session's own advertisement, and RequireRemoteRefs
// refuses the push unless that value is the tracking ref's hash. A tip
// someone else pushed AFTER this instance last saw the branch is therefore
// never deleted; one it fetched before the retract is (the loop force-pushes
// its own agent branch anyway). Such a push is refused
// with an error that names both hashes, and the origin remote is kept so that
// the caller can retry.
//
// A branch that is already absent on the origin is not an error. When the
// origin has the branch but this instance has no tracking ref for it, this
// instance never pushed it: nothing is deleted, and the call fails.
func (ri *remoteIndex) RetractPushedBranch(ctx context.Context, branch string, auth transport.AuthMethod) error {
	if branch == "" {
		return ErrNoAgentBranch
	}
	defer ri.rh.lockPush(branch)()
	remote, err := ri.rh.repo.Remote("origin")
	if errors.Is(err, gogit.ErrRemoteNotFound) {
		return nil // nothing can push here any more, and a re-run after a removal lands here
	}
	if err != nil {
		return fmt.Errorf("RetractPushedBranch: %w", err)
	}
	name := plumbing.NewBranchReferenceName(branch)
	tracking := plumbing.NewRemoteReferenceName("origin", branch)
	var pushed plumbing.Hash
	if ref, err := ri.rh.gits.Reference(tracking); err == nil {
		pushed = ref.Hash()
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("RetractPushedBranch: %s: %w", tracking, err)
	}

	netCtx, cancel := ri.rh.netCtx(ctx)
	defer cancel()
	refs, err := remote.ListContext(netCtx, &gogit.ListOptions{Auth: auth})
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return fmt.Errorf("RetractPushedBranch: list the origin: %w", err)
	}
	var at plumbing.Hash
	for _, r := range refs {
		if r.Name() == name {
			at = r.Hash()
		}
	}
	switch {
	case at.IsZero():
		// Absent on the origin: nothing to delete.
	case pushed.IsZero():
		return fmt.Errorf("RetractPushedBranch: the origin's %s is at %s, and this instance never pushed it: left alone", branch, at)
	case at != pushed:
		return fmt.Errorf("RetractPushedBranch: the origin's %s is at %s, not %s (the commit this instance pushed): left alone", branch, at, pushed)
	default:
		var srv bytes.Buffer
		if err := ri.rh.repo.PushContext(netCtx, &gogit.PushOptions{
			RemoteName:        "origin",
			RefSpecs:          []gogitconfig.RefSpec{gogitconfig.RefSpec(":" + name.String())},
			RequireRemoteRefs: []gogitconfig.RefSpec{gogitconfig.RefSpec(pushed.String() + ":" + name.String())},
			Auth:              auth,
			Progress:          &srv,
		}); err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
			return fmt.Errorf("RetractPushedBranch: delete %s on the origin: %w%s", branch, err, remoteSays(srv.String()))
		}
		log.Info().Str("branch", branch).Str("commit", pushed.String()).Msg("RetractPushedBranch: deleted on the origin")
	}
	if err := ri.rh.gits.RemoveReference(tracking); err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("RetractPushedBranch: %s: %w", tracking, err)
	}
	if err := ri.rh.repo.DeleteRemote("origin"); err != nil && !errors.Is(err, gogit.ErrRemoteNotFound) {
		return fmt.Errorf("RetractPushedBranch: remove the origin remote: %w", err)
	}
	return nil
}

// storeHooks are test seams for the store. Every field is nil in production.
type storeHooks struct {
	// afterSnapshot runs in Push after the snapshot is taken and the write
	// lock released, before any network: a test parks a push here to show
	// that writes proceed while it is "on the network", and that a commit
	// landing meanwhile is not what this push sends.
	afterSnapshot func(branch string, snap plumbing.Hash)
	// Failure seams for the snapshot-then-reconcile tests: a non-nil error
	// fails the Sync round at that step, as a crash there would.
	afterFetch           func() error            // after a successful fetch
	afterMainReconcile   func(mode string) error // after the main reconcile, before the agent's
	beforeOriginSnapshot func() error            // after the reconcile, before the snapshot write
	// beforePush fails a Push before any network.
	beforePush func(branch string) error
}

var (
	storeHooksMu   sync.Mutex
	storeTestHooks storeHooks
)

func currentStoreHooks() storeHooks {
	storeHooksMu.Lock()
	defer storeHooksMu.Unlock()
	return storeTestHooks
}

// remoteSays renders the server's own words from a push's sideband, ready to
// append to an error.
//
// git sends hook output back prefixed "remote: ", interleaved with progress
// that uses carriage returns, and hosts repeat themselves (GitLab frames its
// reason with blank banner lines above and below). So: split on both line
// endings, strip the prefix, drop blanks and duplicates.
//
// The lines are kept as LINES. Hosts wrap their prose and format instructions
// as lists — GitLab answers a refused initial commit with a sentence and two
// numbered remedies — and folding that onto one line with separators turns
// readable guidance into a run-on. Callers render it in a pre-wrap block.
//
// Returns "" when the server said nothing, so the caller's error reads exactly
// as it did before rather than trailing an empty separator.
func remoteSays(sideband string) string {
	var lines []string
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(sideband, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "remote:"))
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n")
}

// ErrUpstreamMoved is PublishUpstreamChange's answer when the origin's
// upstream is no longer the tip the change was built on (the push is not a
// fast-forward): someone else published first.
var ErrUpstreamMoved = errors.New("the origin's upstream moved: the change is not a fast-forward")

// PublishUpstreamChange commits ONE change on top of the local upstream
// branch's tip (path set to *content, or deleted when content is nil), signed
// by this store's signer and authored as this instance, and pushes that
// commit to the origin's upstream branch WITHOUT force. Only when the origin
// took it does the local upstream follow (as a fetch would). The agent branch
// is never touched: a refused push leaves nothing but an unreachable commit
// object, so the caller has nothing to undo. The local upstream should be
// fresh (a fetch this round): a push from a stale tip is refused by the
// remote as not a fast-forward, which is the answer "someone else published
// first".
//
// It is the fleet's way to publish the master marker on a forge (claim,
// stand-down, decommission: repos claimFleetMaster and friends): git decides
// a race between two claims, the first publish wins.
func (ri *remoteIndex) PublishUpstreamChange(ctx context.Context, from, path string, content *string, message string, auth transport.AuthMethod) (plumbing.Hash, error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: no origin: %v", err)
	}
	upstream := remote.Branch
	if upstream == "" {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: the origin has no branch: %w", ErrNoConsensusBranch)
	}
	rh := ri.rh
	signer, err := rh.commitSigner()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	defer rh.lockPush(upstream)()
	parent, err := rh.resolveRef(ctx, upstream)
	if err != nil || parent == plumbing.ZeroHash {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: local %s: %v", upstream, err)
	}
	pc, err := object.GetCommit(rh.gits, parent)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	tree, err := pc.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	var root plumbing.Hash
	if content != nil {
		blob := rh.gits.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		w, err := blob.Writer()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if _, err := io.WriteString(w, *content); err != nil {
			w.Close()
			return plumbing.ZeroHash, err
		}
		w.Close()
		bh, err := rh.gits.SetEncodedObject(blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if root, err = buildTree(rh.gits, tree, path, bh); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
		}
	} else if root, err = deleteFromTree(rh.gits, tree, path); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	if root == tree.Hash {
		return parent, nil // nothing to change
	}
	author, committer, err := rh.commitSigs(ctx, upstream, "updated")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	c := &object.Commit{Author: author, Committer: committer, Message: message, TreeHash: root, ParentHashes: []plumbing.Hash{parent}}
	h, err := storeCommit(rh.gits, signer, c)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	// The one commit this publishes onto main is judged first
	// (judgeOwnRange); a refused one is left unreachable and nothing moves.
	if err := rh.judgeOwnRange(upstream, parent, h, ri.ownID(upstream), ri.branchTip(from)); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w", err)
	}
	pushCtx, cancel := rh.netCtx(ctx)
	defer cancel()
	var srv bytes.Buffer
	if err := rh.repo.PushContext(pushCtx, &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(fmt.Sprintf("%s:refs/heads/%s", h, upstream))},
		Auth:       auth,
		Progress:   &srv,
	}); err != nil {
		if strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(srv.String(), "non-fast-forward") {
			return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w: %v%s", ErrUpstreamMoved, err, remoteSays(srv.String()))
		}
		return plumbing.ZeroHash, fmt.Errorf("PublishUpstreamChange: %w%s", err, remoteSays(srv.String()))
	}
	unlock := rh.lockBranch(upstream)
	_, _, err = rh.advanceBranchTo(context.WithoutCancel(ctx), upstream, h)
	unlock()
	if err != nil {
		log.Warn().Err(err).Str("upstream", upstream).Msg("PublishUpstreamChange: pushed; the local upstream did not follow yet (the next fetch moves it)")
	}
	log.Info().Str("upstream", upstream).Str("path", path).Str("commit", h.String()).Msg("PublishUpstreamChange: pushed")
	return h, nil
}
