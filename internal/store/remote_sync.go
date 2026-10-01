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
	"strings"
	"sync"
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
// upstreamMain is the consensus branch, never empty here: every caller has
// resolved it (InitFromRemote, InitSubscription) or refused an empty one (Sync).
func fetchOrigin(ctx context.Context, repo *gogit.Repository, auth transport.AuthMethod, upstreamMain string, timeout time.Duration) error {
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
// upstreamMain is the consensus branch name; Sync, the only production
// caller, refuses an empty one before getting here.
func (ri *remoteIndex) reconcileNow(ctx context.Context, agentBranch, upstreamMain string) (SyncResult, error) {

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
	agentRes, err := ri.rh.reconcileAgent(ctx, agentBranch, upstreamMain, strategy, mainRes.Mode == ModeRewound)
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
// loop, the fleet register/unregister push and the create-time push — so one
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

// storeHooks are test seams for the store. Every field is nil in production.
type storeHooks struct {
	// afterSnapshot runs in Push after the snapshot is taken and the write
	// lock released, before any network: a test parks a push here to show
	// that writes proceed while it is "on the network", and that a commit
	// landing meanwhile is not what this push sends.
	afterSnapshot func(branch string, snap plumbing.Hash)
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
