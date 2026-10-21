package remotesync

// Remote bootstrap: the constructors that turn an empty store into a clone of,
// or a subscription to, a remote (CloneFrom, InitFromRemote, InitSubscription).
// They are methods of Index, reached as Service.Remote(); the contract docs are
// on them. Local creation (OpenRepo, InitRepo, InitRepoWithUpstream) stays in
// the parent.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/verify"
)

// ErrEmptyRemote is returned by CloneFrom when the remote repository exists
// but contains no branches/commits. Knomit's sync model requires the remote
// to have at least one branch (typically "main") to merge into the agent
// branch, so an empty remote cannot be connected.
var ErrEmptyRemote = errors.New("remote repository has no branches")

// progressWriter adapts a progress callback to io.Writer for use with CloneOptions.Progress.
type progressWriter struct {
	fn func(string)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.fn(string(p))
	return len(p), nil
}

// progressSink adapts an optional progress callback to the sideband.Progress
// (io.Writer) that go-git's FetchOptions wants.
//
// A nil callback must yield a nil WRITER, never a writer wrapping nil: go-git
// treats a non-nil Progress as "this caller wants progress", and on the PUSH
// path that decides whether side-band is requested at all. Returning a
// non-nil-but-useless writer would therefore change the protocol for every
// caller that passes nil, which is most of them.
//
// (On the FETCH path go-git asks for side-band whenever the server advertises
// it, Progress or not — remote.go's NewUploadRequestFromCapabilities. A nil
// Progress there means the demuxer drops band 2 on the floor, which is exactly
// right for a caller that did not ask.)
func progressSink(fn func(string)) sideband.Progress {
	if fn == nil {
		return nil
	}
	return &progressWriter{fn: fn}
}

// CloneFrom clones a remote URL into the store this index belongs to.
// Returns ErrEmptyRemote when the remote exists but has no branches yet —
// knomit's sync model requires at least one branch on the remote.
func (ri *Index) CloneFrom(url string, auth transport.AuthMethod, progress func(string)) error {
	rh := ri.rh
	opts := &gogit.CloneOptions{
		URL:  url,
		Auth: auth,
	}
	if progress != nil {
		opts.Progress = &progressWriter{fn: progress}
	}

	ctx, cancel := rh.vr.NetCtx(context.Background())
	defer cancel()
	repo, cloneErr := gogit.CloneContext(ctx, rh.vr.Storer(), memfs.New(), opts)
	if errors.Is(cloneErr, transport.ErrEmptyRemoteRepository) {
		return ErrEmptyRemote
	}
	if cloneErr != nil {
		return fmt.Errorf("CloneFrom: clone: %w", cloneErr)
	}

	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("CloneFrom: resolve HEAD: %w", err)
	}
	branch := strings.TrimPrefix(head.Name().String(), "refs/heads/")

	log.Info().Str("branch", branch).Str("url", url).Msg("cloned remote into storer")
	rh.vr.SetRepository(repo)
	return nil
}

// BranchACreateReads names the branch a create against a remote will end up
// READING — the one whose tree decides whether the remote is already a
// knowledge base, and whose tip the repo's agent branch starts from.
//
// THIS IS THE RULE, and it exists as one function because it has to be applied
// in two places that see different data. InitFromRemote applies it below,
// against the remote-tracking refs it has just fetched. repos.ProbeInitialized
// applies it BEFORE any fetch, against the refs the remote advertises, in order
// to predict what a create would do.
//
// When those two answered differently, re-creating a repository that this
// machine had already initialized became a permanent dead end: the probe looked
// at the consensus branch, which knomit never writes to and which therefore
// never gains an ontology, while the create adopted the agent branch, which
// already had one. The wizard could only derive the mode that could not
// succeed. A comment asking two implementations to agree is what allowed that;
// one function is what prevents it.
//
// Only THIS machine's agent branch is adopted. Another machine's is ignored —
// we would cut our own from the consensus branch — which is why the caller
// passes the answer for its own agent branch alone.
func BranchACreateReads(remoteHasAgentBranch bool, agentBranch, consensusBranch string) string {
	if remoteHasAgentBranch {
		return agentBranch
	}
	return consensusBranch
}

// InitOption changes how InitFromRemote bootstraps.
type InitOption func(*initOptions)

type initOptions struct{ ownBranchesOnly bool }

// OwnBranchesOnly makes the bootstrap fetch bring every branch of the remote
// EXCEPT other instances' agent/* branches: the fleet door's clone
// (R-visibility: a non-master never gets another agent's branch, and that
// includes its first clone, PR #524 S2). Without it the bootstrap fetch is
// the wildcard +refs/heads/*:refs/remotes/origin/*, which copies every agent
// branch's objects into the store for good, even though the steady-state
// refspecs never fetch them again.
func OwnBranchesOnly() InitOption { return func(o *initOptions) { o.ownBranchesOnly = true } }

// ownBranchesRefspecs lists the remote's branches (one advertisement, no
// objects) and returns one exact refspec per branch that is not an agent/*
// branch, plus own when the remote has it. A remote with no ref answers
// transport.ErrEmptyRemoteRepository; one whose only branches are other
// agents' answers vcs.ErrNoConsensusBranch (an empty list would fall back to the
// wildcard).
func ownBranchesRefspecs(ctx context.Context, originURL string, auth transport.AuthMethod, own string, timeout time.Duration) ([]gogitconfig.RefSpec, error) {
	rem := gogit.NewRemote(memory.NewStorage(), &gogitconfig.RemoteConfig{Name: "origin", URLs: []string{originURL}})
	lctx, cancel := vcs.NetCtxWith(ctx, timeout)
	defer cancel()
	refs, err := rem.ListContext(lctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return nil, err
	}
	var out []gogitconfig.RefSpec
	for _, ref := range refs {
		if !ref.Name().IsBranch() {
			continue
		}
		b := ref.Name().Short()
		if strings.HasPrefix(b, "agent/") && b != own {
			continue
		}
		out = append(out, gogitconfig.RefSpec("+refs/heads/"+b+":refs/remotes/origin/"+b))
	}
	if len(refs) == 0 {
		return nil, transport.ErrEmptyRemoteRepository
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the remote holds only other agents' branches", vcs.ErrNoConsensusBranch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// InitFromRemote initializes a knomit git repo by fetching from a remote origin.
//
// upstreamMain selects the remote's consensus branch. When empty, it is
// resolved from the remote after the initial fetch by ChooseConsensusBranch
// (its HEAD unless that is an agent branch, else its one other branch), and a
// remote that answers neither is refused with ErrNoConsensusBranch.
//
// The resolved upstream is RETURNED so the caller can persist the branch the
// clone actually adopted rather than the one it asked for. Callers that pass
// "" and then persist their own fallback would silently rewrite a trunk- or
// master-convention remote to some other name: the refspecs and remotes row
// would disagree with the branch this function created locally. On success it
// also RECORDS that branch as the repo's consensus branch (RecordConsensusBranch),
// deferred, so the record exists as soon as this returns and before any
// SetOrigin.
//
// If the remote has an existing agent branch for this hostname, it is used.
// Otherwise a new agent branch is created from origin/<upstreamMain>.
// If the remote is empty (no refs), falls back to creating initial content
// inline — initFiles are written as seed files on the new agent branch in
// that case, and are ignored when the remote already has branches.
//
// remoteWasEmpty REPORTS WHICH OF THOSE TWO PATHS RAN, and is the only way a
// caller can know: the dispatch happens inside, on the fetch's
// transport.ErrEmptyRemoteRepository, long after any pre-check the caller ran.
// true means the empty path (initFiles were written, the remote had no refs at
// FETCH time); false means the clone path (the remote had refs, initFiles were
// ignored). A caller whose meaning depends on which path ran must ACT on this
// rather than trust its own pre-check, which was taken strictly earlier in
// time. Both repos-layer remote modes do: repos.initClone and
// repos.initInitialize each refuse when this comes back true, because the
// empty path mints a fresh ROOT COMMIT — and therefore a repo identity no
// other machine sharing that remote would agree with. On a non-nil error the
// value is meaningless.
func (ri *Index) InitFromRemote(ctx context.Context, originURL string, auth transport.AuthMethod, upstreamMain, agentBranch string, initFiles map[string]string, progress func(string), opts ...InitOption) (upstream string, remoteWasEmpty bool, err error) {
	rh := ri.rh
	// Record the RESOLVED consensus branch (consensus_branch.go): it outlives
	// the origin row if the origin is later removed.
	defer func() {
		if err == nil {
			ri.RecordConsensusBranch(upstream)
		}
	}()
	repo, err := gogit.Init(rh.vr.Storer(), memfs.New())
	if err != nil {
		return "", false, fmt.Errorf("InitFromRemote: git init: %w", err)
	}

	if err := vcs.InitRepoConfig(repo, "InitFromRemote"); err != nil {
		return "", false, err
	}

	var o initOptions
	for _, opt := range opts {
		opt(&o)
	}
	bootstrap := []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"}
	if o.ownBranchesOnly {
		if agentBranch == "" {
			agentBranch = vcs.DefaultAgentBranch()
		}
		bootstrap, err = ownBranchesRefspecs(ctx, originURL, auth, agentBranch, rh.vr.NetTimeout())
		if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return "", false, fmt.Errorf("InitFromRemote: list the remote: %w", err)
		}
	}
	_, cerr := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{originURL},
		Fetch: bootstrap,
	})
	if cerr != nil {
		return "", false, fmt.Errorf("InitFromRemote: create remote: %w", cerr)
	}

	if err == nil {
		fetchCtx, fetchCancel := rh.vr.NetCtx(ctx)
		err = repo.FetchContext(fetchCtx, &gogit.FetchOptions{
			RemoteName: "origin",
			Auth:       auth,
			Progress:   progressSink(progress),
		})
		fetchCancel()
	}
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		up, seedErr := initFromEmptyRemote(ctx, rh, repo, originURL, auth, upstreamMain, agentBranch, initFiles)
		return up, true, seedErr
	}
	if err != nil {
		return "", false, fmt.Errorf("InitFromRemote: fetch: %w", err)
	}

	if agentBranch == "" {
		agentBranch = vcs.DefaultAgentBranch()
	}

	upstreamMain, err = resolveUpstream(ctx, rh, repo, auth, upstreamMain)
	if err != nil {
		return "", false, fmt.Errorf("InitFromRemote: %w", err)
	}

	// Publish the freshly-initialised repo on the handler BEFORE
	// ConfigureRemote — that helper reaches into rh.repo to read and rewrite
	// the git config, and would panic on nil otherwise. The rest of
	// InitFromRemote also depends on rh.repo being live for the remote-
	// tracking ref lookups below.
	rh.vr.SetRepository(repo)

	// Configure remote with two-refspec fetch (main + agent/<host>).
	// The initial CreateRemote above used a wildcard refspec (or, under
	// OwnBranchesOnly, every branch but other agents') to discover the remote's
	// branches at bootstrap; now we lock it down for steady state.
	if err := rh.vr.ConfigureRemote(originURL, upstreamMain, agentBranch); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: configure remote: %w", err)
	}

	// Re-fetch with the proper refspec so origin/<upstreamMain> and
	// origin/agent/<host> are both tracked consistently. (The initial
	// wildcard fetch already pulled objects; this just establishes the
	// remote-tracking refs under the new refspec shape.) Use FetchOrigin so
	// the agent ref's absence on origin (typical first connect) is tolerated.
	if err := FetchOrigin(ctx, repo, auth, upstreamMain, rh.vr.NetTimeout()); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: re-fetch: %w", err)
	}

	// Bootstrap local upstream branch from origin/<upstreamMain>. Origin's
	// main is trusted as fetched (F09 verifies once, at the gate that
	// advanced it).
	originMainRef, err := rh.vr.Storer().Reference(plumbing.NewRemoteReferenceName("origin", upstreamMain))
	if err != nil {
		return "", false, fmt.Errorf("InitFromRemote: resolve origin/%s: %w", upstreamMain, err)
	}

	// E4 (F09, every mode, off included), BEFORE any local ref is set:
	// origin's copy of THIS instance's agent branch is adopted only if every
	// commit it holds beyond origin's upstream is signed by this
	// instance's own key (or accepted on this instance). Otherwise the create
	// FAILS with a ForeignLineageError naming the commits and the remedies. It
	// never starts a fresh lineage under that name: that would overwrite the
	// remote branch on the first push and silently discard its commits.
	if agentBranch == "" {
		agentBranch = vcs.DefaultAgentBranch()
	}
	if remoteAgent, rerr := rh.vr.Storer().Reference(plumbing.NewRemoteReferenceName("origin", agentBranch)); rerr == nil {
		// rh.vr is the one shared *vcs.Repo, reached through
		// remotesync.Repo -> merge.Repo -> vcs.Repo.
		if e4err := verify.CheckOwnLineage(ctx, rh.vr, agentBranch, remoteAgent.Hash(), originMainRef.Hash()); e4err != nil {
			return "", false, fmt.Errorf("InitFromRemote: %w", e4err)
		}
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), originMainRef.Hash())); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: set local %s: %w", upstreamMain, err)
	}

	// Bootstrap local agent and compute the watermark in the same branch
	// so the origin/agent lookup happens exactly once.
	//
	// If origin/agent/<host> exists, adopt it: the local agent ref points
	// at the adopted tip, and the watermark must be the main commit that
	// chain last branched from, i.e. MergeBase(origin/agent, origin/main).
	// Seeding the watermark to current origin/main would be wrong when
	// origin/main has advanced past the last push — the current main
	// commit is not on origin/agent's first-parent chain, so the next
	// reconcile's unpushedCommits walk would reach root and replay every
	// ancestor (including old main commits), resurrecting files that
	// main has since deleted under StrategyLocalWins.
	//
	// If origin/agent/<host> does NOT exist, bootstrap from origin/main:
	// the agent ref IS origin/main, so the watermark equals it.
	//
	// Watermark = the main commit the agent's chain last branched from.
	// For bootstrap-from-main this equals origin/main. For adoption of an
	// existing origin/agent, this equals MergeBase(origin/agent, origin/main).
	// The watermark commit must be on agent's first-parent chain so the
	// unpushedCommits walk stops there cleanly.
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)
	var watermarkHash plumbing.Hash
	remoteAgentRef, remoteAgentErr := rh.vr.Storer().Reference(plumbing.NewRemoteReferenceName("origin", agentBranch))
	// The rule, applied here against the refs just fetched. ProbeInitialized
	// applies the SAME function against the refs the remote advertises, so its
	// prediction of what this create will read cannot drift from what it does.
	if BranchACreateReads(remoteAgentErr == nil, agentBranch, upstreamMain) == agentBranch {
		// Adopt path: agent ref points at the adopted origin/agent tip.
		if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(agentRefName, remoteAgentRef.Hash())); err != nil {
			return "", false, fmt.Errorf("InitFromRemote: set agent from remote agent: %w", err)
		}
		remoteAgentCommit, err := rh.vr.Repository().CommitObject(remoteAgentRef.Hash())
		if err != nil {
			return "", false, fmt.Errorf("InitFromRemote: load remote agent commit: %w", err)
		}
		originMainCommit, err := rh.vr.Repository().CommitObject(originMainRef.Hash())
		if err != nil {
			return "", false, fmt.Errorf("InitFromRemote: load origin main commit: %w", err)
		}
		bases, err := remoteAgentCommit.MergeBase(originMainCommit)
		if err != nil {
			return "", false, fmt.Errorf("InitFromRemote: merge-base(remote agent, origin main): %w", err)
		}
		if len(bases) == 0 {
			// Disjoint histories — fall back to current origin/main and
			// let the next reconcile's disjoint-replay handle it. Shape
			// matches scenario G2.
			watermarkHash = originMainRef.Hash()
		} else {
			watermarkHash = bases[0].Hash
		}
	} else {
		// Bootstrap-from-main path: agent ref is origin/main, watermark
		// equals it.
		if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(agentRefName, originMainRef.Hash())); err != nil {
			return "", false, fmt.Errorf("InitFromRemote: set agent from main: %w", err)
		}
		watermarkHash = originMainRef.Hash()
	}

	if err := rh.vr.Storer().SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: set HEAD: %w", err)
	}

	if err := rh.WriteAgentBase(agentBranch, watermarkHash); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: seed agent watermark: %w", err)
	}

	log.Info().Str("branch", agentBranch).Str("upstream", upstreamMain).Str("origin", originURL).Msg("git store initialized from remote")
	// The shared *vcs.Repo already has its go-git repository published (set
	// earlier so ConfigureRemote could run).
	if _, err := rh.vr.EnsureBranch(ctx, agentBranch, "refs/heads/"+agentBranch); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := rh.vr.EnsureBranch(ctx, upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: ensure upstream branch %q: %w", upstreamMain, err)
	}
	// History is derived before the repo serves anything (OpenHistory); a
	// failure keeps it closed, and the next open retries.
	if err := rh.vr.OpenHistory(ctx); err != nil {
		return "", false, fmt.Errorf("InitFromRemote: history: %w", err)
	}
	if err := rh.vr.PopulateCommitLog(ctx, agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: remote populate failed")
	}
	if err := rh.vr.PopulateCommitLog(ctx, upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: remote populate (upstream) failed")
	}
	// remoteWasEmpty=false: this is the CLONE path — the fetch above found refs.
	return upstreamMain, false, nil
}

// resolveUpstream applies the consensus-branch rule shared by every remote
// init path, ChooseConsensusBranch: a requested name; else the remote's
// symbolic HEAD when it is not an agent/experiment/generated branch (a remote
// whose HEAD points at an agent branch must NOT make that our consensus); else
// the one fetched branch with no other role. Anything else is refused with
// vcs.ErrNoConsensusBranch naming the candidates, never resolved by a branch's
// name. Must run AFTER the wildcard fetch so the remote-tracking refs exist.
func resolveUpstream(ctx context.Context, rh *Repo, repo *gogit.Repository, auth transport.AuthMethod, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	branches := remoteTrackingBranches(repo)
	head := detectRemoteUpstream(ctx, repo, auth, rh.vr.NetTimeout())
	if b := vcs.ChooseConsensusBranch("", head, branches); b != "" {
		log.Info().Str("upstream", b).Str("remote_head", head).Msg("resolveUpstream: adopted the remote's consensus branch")
		return b, nil
	}
	return "", fmt.Errorf("%w: the remote's HEAD is %q and its branches are %v; name the consensus branch", vcs.ErrNoConsensusBranch, head, branches)
}

// remoteTrackingBranches lists the branches the wildcard fetch brought in, by
// their refs/remotes/origin/* names, sorted.
func remoteTrackingBranches(repo *gogit.Repository) []string {
	iter, err := repo.References()
	if err != nil {
		return nil
	}
	defer iter.Close()
	const prefix = "refs/remotes/origin/"
	var out []string
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		n := ref.Name().String()
		if strings.HasPrefix(n, prefix) && n != prefix+"HEAD" {
			out = append(out, strings.TrimPrefix(n, prefix))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// InitSubscription initialises a repo that FOLLOWS a remote branch read-only.
//
// It is InitFromRemote without the agent half: fetch, resolve the consensus
// branch by the same rule (resolveUpstream), track that branch alone, and set
// the local branch and HEAD to its tip. No agent branch is cut, no watermark
// is written, and nothing here ever pushes. Sync for such a repo is
// reconcileMain alone (remoteIndex.reconcileNow with an empty agent branch).
//
// The resolved upstream is RETURNED for the same reason InitFromRemote returns
// it: the caller persists what was adopted, never what it asked for.
//
// A remote with no refs is refused with transport.ErrEmptyRemoteRepository:
// there is no branch to follow, and unlike InitFromRemote there is no
// seed-locally fallback because a subscription owns no content.
func (ri *Index) InitSubscription(ctx context.Context, originURL string, auth transport.AuthMethod, upstreamMain string, progress func(string)) (string, error) {
	rh := ri.rh
	repo, err := gogit.Init(rh.vr.Storer(), memfs.New())
	if err != nil {
		return "", fmt.Errorf("InitSubscription: git init: %w", err)
	}
	if err := vcs.InitRepoConfig(repo, "InitSubscription"); err != nil {
		return "", err
	}
	if _, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{originURL},
		Fetch: []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/origin/*"},
	}); err != nil {
		return "", fmt.Errorf("InitSubscription: create remote: %w", err)
	}

	fetchCtx, fetchCancel := rh.vr.NetCtx(ctx)
	err = repo.FetchContext(fetchCtx, &gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		Progress:   progressSink(progress),
	})
	fetchCancel()
	if err != nil {
		return "", fmt.Errorf("InitSubscription: fetch: %w", err)
	}

	upstreamMain, err = resolveUpstream(ctx, rh, repo, auth, upstreamMain)
	if err != nil {
		return "", fmt.Errorf("InitSubscription: %w", err)
	}

	// Publish before ConfigureRemote, which reads and rewrites rh.repo's config.
	rh.vr.SetRepository(repo)
	if err := rh.vr.ConfigureRemote(originURL, upstreamMain, ""); err != nil {
		return "", fmt.Errorf("InitSubscription: configure remote: %w", err)
	}
	if err := FetchOrigin(ctx, repo, auth, upstreamMain, rh.vr.NetTimeout()); err != nil {
		return "", fmt.Errorf("InitSubscription: re-fetch: %w", err)
	}

	originRef, err := rh.vr.Storer().Reference(plumbing.NewRemoteReferenceName("origin", upstreamMain))
	if err != nil {
		return "", fmt.Errorf("InitSubscription: resolve origin/%s: %w", upstreamMain, err)
	}
	localName := plumbing.NewBranchReferenceName(upstreamMain)
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(localName, originRef.Hash())); err != nil {
		return "", fmt.Errorf("InitSubscription: set local %s: %w", upstreamMain, err)
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, localName)); err != nil {
		return "", fmt.Errorf("InitSubscription: set HEAD: %w", err)
	}
	ri.RecordConsensusBranch(upstreamMain)

	if _, err := rh.vr.EnsureBranch(ctx, upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return "", fmt.Errorf("InitSubscription: ensure upstream branch %q: %w", upstreamMain, err)
	}
	// History is derived before the repo serves anything (OpenHistory); a
	// failure keeps it closed, and the next open retries.
	if err := rh.vr.OpenHistory(ctx); err != nil {
		return "", fmt.Errorf("InitSubscription: history: %w", err)
	}
	if err := rh.vr.PopulateCommitLog(ctx, upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: subscription populate failed")
	}
	log.Info().Str("upstream", upstreamMain).Str("origin", originURL).Msg("git store initialized as a subscription")
	return upstreamMain, nil
}

// detectRemoteUpstream queries origin's symbolic HEAD to determine the default
// branch. Returns "" when detection fails; ChooseConsensusBranch then decides
// from the fetched branches alone.
//
// Uses remote.List (which issues a separate ls-remote round-trip) rather than
// reading a local ref, because go-git's default fetch refspecs do not bring
// HEAD into refs/remotes/origin/. The extra round-trip happens at repo init
// only.
func detectRemoteUpstream(ctx context.Context, repo *gogit.Repository, auth transport.AuthMethod, timeout time.Duration) string {
	remote, err := repo.Remote("origin")
	if err != nil {
		return ""
	}
	return detectFromRemote(ctx, remote, auth, timeout)
}

// Detection from a bare URL (a throwaway in-memory repo with `origin` attached,
// queried for its symbolic HEAD) used to exist here for the repos builder, which
// resolved the upstream BEFORE cloning. Nothing needs that ordering now:
// InitFromRemote fetches first and detects against the real repo, which is
// strictly better: it can see which branches exist beside whatever HEAD points
// at (an agent-branch HEAD is skipped), and it reports what it resolved.

func detectFromRemote(ctx context.Context, remote *gogit.Remote, auth transport.AuthMethod, timeout time.Duration) string {
	ctx, cancel := vcs.NetCtxWith(ctx, timeout)
	defer cancel()
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return ""
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return strings.TrimPrefix(ref.Target().String(), "refs/heads/")
		}
	}
	return ""
}

// initFromEmptyRemote handles the empty-remote fallback for InitFromRemote.
// initFiles are written as additional seed files on top of the root manifest
// so the new agent branch matches the layout produced by InitRepo.
// upstreamMain is the consensus branch name to bootstrap. The empty-remote
// path has no remote HEAD to detect from and CREATES the branch, so the
// caller's value is authoritative and an empty one gets DefaultConsensusBranch.
// It is returned for the same reason InitFromRemote returns it: the caller
// persists what was bootstrapped, not what it requested.
func initFromEmptyRemote(ctx context.Context, rh *Repo, repo *gogit.Repository, originURL string, auth transport.AuthMethod, upstreamMain, agentBranch string, initFiles map[string]string) (string, error) {
	if upstreamMain == "" {
		upstreamMain = DefaultConsensusBranch
	}
	rootManifest := "# Knowledge Base\n\nRoot manifest.\n"
	initSig := object.Signature{Name: "knomit", Email: "knomit@local", When: time.Now()}
	// The nonce guarantees distinct root-commit hashes for repos created in
	// the same second (git timestamps have second precision and everything
	// else in this commit is fixed) — the root hash is the repo's stable
	// identity (lenses RFC decision 11).
	//
	// ACCEPTED RACE — split-brain on a shared empty remote: if two machines both
	// observe this origin as empty (the ListContext check upstream) and both run
	// this fallback, each mints a distinct nonce → distinct root commit → distinct
	// repo identity. Binding.ByID then treats them as two repos even though the
	// user believes they are one. There is no push-time signal that detects this:
	// Push force-pushes only the per-host agent ref (agent/<host>, sync/remote_sync.go),
	// so the second machine writes a DIFFERENT ref name — no collision, no
	// non-fast-forward reject, the divergence is silent. Agents never push the
	// consensus branch, so there is no main-ref collision to catch either. We do
	// NOT weaken the nonce: independently-created repos MUST differ (decision 11),
	// and this is the deliberate flip side. A real fix is out of scope here — it
	// requires cross-machine coordination at init (re-list the remote after the
	// empty check and adopt an existing knomit root instead of minting one, or a
	// first-writer-wins lock on origin), NOT automatic reconciliation after the
	// fact.
	initMsg := fmt.Sprintf("init: create knowledge base\n\nknomit-repo-nonce: %s", uuid.New().String())
	lastCommit, _, writeErr := vcs.WriteFileToStore(rh.vr.Storer(), nil, plumbing.ZeroHash, "README.md", rootManifest, initMsg, initSig, initSig)
	if writeErr != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote fallback: %w", writeErr)
	}
	for path, content := range initFiles {
		lastCommit, _, writeErr = vcs.WriteFileToStore(rh.vr.Storer(), nil, lastCommit, path, content, "init: "+path, initSig, initSig)
		if writeErr != nil {
			return "", fmt.Errorf("InitFromRemote: empty remote write %s: %w", path, writeErr)
		}
	}
	if agentBranch == "" {
		agentBranch = vcs.DefaultAgentBranch()
	}
	agentRefName := plumbing.NewBranchReferenceName(agentBranch)
	if writeErr = rh.vr.Storer().SetReference(plumbing.NewHashReference(agentRefName, lastCommit)); writeErr != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote set agent ref: %w", writeErr)
	}
	if writeErr = rh.vr.Storer().SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); writeErr != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote set HEAD: %w", writeErr)
	}
	if writeErr = rh.vr.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(upstreamMain), lastCommit)); writeErr != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote set %s: %w", upstreamMain, writeErr)
	}
	// Seed the watermark — same shape as the non-empty remote bootstrap.
	if writeErr = rh.WriteAgentBase(agentBranch, lastCommit); writeErr != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote seed agent watermark: %w", writeErr)
	}
	log.Info().Str("branch", agentBranch).Str("origin", originURL).Msg("git store initialized (empty remote)")
	rh.vr.SetRepository(repo)
	if _, err := rh.vr.EnsureBranch(ctx, agentBranch, "refs/heads/"+agentBranch); err != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote ensure agent branch %q: %w", agentBranch, err)
	}
	if _, err := rh.vr.EnsureBranch(ctx, upstreamMain, "refs/heads/"+upstreamMain); err != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote ensure upstream branch %q: %w", upstreamMain, err)
	}
	// History is derived before the repo serves anything (OpenHistory); a
	// failure keeps it closed, and the next open retries.
	if err := rh.vr.OpenHistory(ctx); err != nil {
		return "", fmt.Errorf("InitFromRemote: empty remote history: %w", err)
	}
	if err := rh.vr.PopulateCommitLog(ctx, agentBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: empty-remote populate failed")
	}
	if err := rh.vr.PopulateCommitLog(ctx, upstreamMain); err != nil {
		log.Warn().Err(err).Str("branch", upstreamMain).Msg("commit_log: empty-remote populate (upstream) failed")
	}
	return upstreamMain, nil
}
