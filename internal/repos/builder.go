package repos

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// repoBuilder accumulates state while constructing a RepoInstance step by step.
// Each method returns an error; the caller (Manager.openOne) checks after each
// step and can clean up with close() on failure.
type repoBuilder struct {
	// inputs
	name                  string
	uid                   string
	origin                *Origin // from control.db; nil when this repo has no remote
	dbPath                string
	cfg                   config.Config
	signer                ssh.Signer
	agentBranch           string
	embedder              store.BatchEmbedder
	keyPath               string
	ctx                   context.Context
	disableBackgroundSync bool

	// accumulated state
	svc      *store.Service
	ontology *fact.Ontology

	// index work deferred to the background (set by setupIndex, run after
	// build): the branches whose index we maintain, each carrying its own
	// full-rebuild-or-incremental-sync verdict.
	indexBranches []healBranch

	// deferred-activation handles: build() constructs these but does NOT start
	// the sync loops / observer until activate() runs (after the background
	// index), so two writers never race the initial index build.
	hub     *TaskHub
	syncCtx context.Context
	syncWg  *sync.WaitGroup

	// index-heal handles: the background heal owns its OWN context + waitgroup,
	// SEPARATE from syncCtx/syncWg. The heal is cancelled only by a real teardown
	// (shutdown/Close/SwapStore), never by startSync's loop-restart cancel — so a
	// runtime clone-create's ActivateSync cannot kill the in-flight initial index.
	indexCtx context.Context
	indexWg  *sync.WaitGroup
	// upstreamMain is the resolved consensus branch name for this repo's
	// origin (e.g. "main" or "master"), read back from control.db's origin by
	// rehydrateUpstreamMain. EMPTY means this repo has no origin — setupIndex
	// relies on that, so never default it to "main".
	upstreamMain string
}

// openStore opens the SQLite-backed store and injects the origin control.db
// holds for this repo.
func (b *repoBuilder) openStore() error {
	svc, err := store.Open(b.dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	b.svc = svc
	// Bound every remote git network op (clone/fetch/push/ls-remote) so a
	// stalled remote aborts instead of hanging forever. store.Open does not
	// know about config, so wire the timeout explicitly here.
	svc.SetNetworkTimeout(b.cfg.Git.NetworkTimeout)
	// Same reason: the store decides index membership by location (only the
	// ontology root holds facts) and has no way to learn the configured root.
	svc.SetOntologyRoot(b.cfg.OntologyRoot)
	// The origin must be injected BEFORE openGit: rehydrateUpstreamMain and the
	// fetch refspec both read it there. Credential decryption happened in
	// control.db, so the store needs no Crypt of its own any more.
	if b.origin != nil {
		svc.SetOrigin(&store.Origin{
			URL:        b.origin.URL,
			Branch:     b.origin.Branch,
			AuthMethod: b.origin.AuthMethod,
			AuthToken:  b.origin.AuthToken,
		})
	}
	return nil
}

// openGit opens the git repository backed by the store.
//
// Opening never CREATES a repository. Every repo is born through Manager.Create
// (InitRepo for preset/custom, InitFromRemote for clone), so a .db on disk with
// no git data is a broken repo, not an invitation to seed one — surfacing the
// error keeps that visible instead of silently manufacturing an empty KB under
// the same name.
func (b *repoBuilder) openGit() error {
	if err := b.svc.OpenRepo(); err != nil {
		return fmt.Errorf("open git: %w", err)
	}
	b.rehydrateUpstreamMain()
	// The git-config remote is a DERIVED CACHE of control.db — rewrite it at
	// every open so a changed URL or upstream in control.db takes effect
	// without any migration of the .db itself.
	if b.origin != nil && b.origin.URL != "" {
		if err := b.svc.ConfigureRemote(b.origin.URL, b.upstreamMain, b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).
				Msg("configure git remote from control.db failed; sync may not reach the origin")
		}
	}
	b.svc.SetSigner(b.signer)
	return nil
}

// rehydrateUpstreamMain loads the resolved upstream branch for this repo.
//
// The branch a repo's origin tracks is decided once, at clone time, and
// persisted in control.db. Every subsequent boot must read it back rather than
// assume: both readers — ensureBranch and setupIndex's branch list — otherwise
// fall back to the literal "main", which for a master-convention origin aims
// the startup index sync at a branch that does not exist.
//
// It reads GetRemote rather than b.origin directly so it goes through the same
// origin contract every other reader relies on (mirrors recoverFromOrigin).
// openStore injected the origin BEFORE openGit called this, so it is visible
// here; the repo's own database holds no upstream branch any more.
//
// EMPTY means this repo has no origin. setupIndex relies on that, so never
// default it to "main".
func (b *repoBuilder) rehydrateUpstreamMain() {
	remote, err := b.svc.Remote().GetRemote("origin")
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).
			Msg("read stored remote failed; upstream branch falls back to default")
		return
	}
	if remote != nil && remote.Branch != "" {
		b.upstreamMain = remote.Branch
	}
}

// loadOntology reads the ontology from the repo's agent branch, walking
// fact.OntologyPathsNewestFirst: the canonical path, then each legacy location
// for repos that predate a move (no migration is provided — repos are updated
// by hand). Falls back to the default ontology only if NO rung has content or
// the content is unparseable.
func (b *repoBuilder) loadOntology() {
	if b.svc == nil {
		b.ontology = fact.DefaultOntology()
		return
	}
	// Track the source path: the refresh below writes BACK to it. Always
	// writing the canonical path would leave a legacy repo holding two
	// ontology files, the stale one indistinguishable from the live one.
	paths := fact.OntologyPathsNewestFirst()
	var (
		srcPath string
		content string
	)
	for _, p := range paths {
		result, rerr := b.svc.Facts().ReadFact(context.Background(), b.agentBranch, p, nil)
		if rerr == nil && result.Content != "" {
			srcPath, content = p, result.Content
			break
		}
	}
	if content == "" {
		log.Warn().Str("repo", b.name).
			Msgf("no ontology at %s, using default ontology", strings.Join(paths, ", "))
		b.ontology = fact.DefaultOntology()
		return
	}
	if srcPath != OntologyPath {
		log.Info().Str("repo", b.name).
			Msgf("ontology loaded from the legacy path %s; rename it to %s", srcPath, OntologyPath)
	}
	ont, err := fact.ParseOntology([]byte(content))
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("failed to parse ontology, using default")
		b.ontology = fact.DefaultOntology()
		return
	}

	// Boot-time refresh: if the stored ontology is preset-derived (matches an
	// embedded preset by id) AND is a strict subset of that preset, the repo
	// was initialized against an older preset and is now lagging — upgrade
	// it in place. A strict subset guarantees the preset only adds; the user
	// hasn't diverged with custom topics or rules.
	//
	// If the stored ontology has diverged (added own topics/rules), log a
	// warning so an operator knows an upgrade is available, but leave their
	// version alone — auto-overwriting custom content would lose work.
	if preset := fact.EmbeddedPresetByID(ont.ID); preset != nil {
		if ont.IsSubsetOf(preset) {
			storedY, sErr := ont.Serialize()
			presetY, pErr := preset.Serialize()
			if sErr == nil && pErr == nil && !bytes.Equal(storedY, presetY) {
				log.Info().
					Str("repo", b.name).
					Str("preset_id", ont.ID).
					Str("path", srcPath).
					Msg("ontology refresh: stored is subset of embedded preset; upgrading to latest")
				if _, werr := b.svc.Facts().WriteFact(
					context.Background(),
					b.agentBranch,
					srcPath,
					string(presetY),
					fmt.Sprintf("ontology: refresh to embedded %s preset", ont.ID),
					"updated",
				); werr != nil {
					log.Warn().Err(werr).Str("repo", b.name).Msg("ontology refresh: write failed, keeping stored")
				} else {
					ont = preset
				}
			}
		} else {
			log.Warn().
				Str("repo", b.name).
				Str("preset_id", ont.ID).
				Msg("ontology refresh: stored has diverged from embedded preset; upgrade skipped")
		}
	}

	b.ontology = ont
}

// ensureBranch creates the agent branch if it doesn't already exist.
//
// It does NOT touch the remotes table. The origin record is written once, by
// whichever path attached the origin — initClone at create time, or the
// PUT /api/v1/{repo}/origin handler afterwards — and both persist the branch
// actually resolved against the remote. Re-seeding it on every open is how the
// upstream used to get silently rewritten to "main".
func (b *repoBuilder) ensureBranch() {
	if b.agentBranch == "" {
		return
	}
	ctx := context.Background()
	if err := b.svc.Branches().CreateBranch(ctx, b.agentBranch, b.seedSourceForAgentBranch()); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("branch create/ensure failed")
		return
	}
	b.recordAgentBranchOwner(ctx)
}

// seedSourceForAgentBranch returns the branch CreateBranch should seed this
// instance's agent branch from. Normally that is the agent branch itself: it
// already exists, so CreateBranch no-ops and the source is never read.
//
// The name is agent/<hostname>-<key-fingerprint> (app.agentBranch), so it moves
// if the SSH key is regenerated or the machine is renamed. The database records
// which branch writes to it (AgentBranchOwner); when this instance's name does
// not match that record, it TAKES OVER the repo — CreateBranch seeds the new
// branch from the recorded owner, which carries the accumulated knowledge, and
// recordAgentBranchOwner then claims it. The previous branch is left in place.
//
// Seeding from the recorded owner rather than from HEAD is deliberate. Several
// agents share a repo through its origin and their branches are fetched into
// refs/heads, so HEAD does not reliably identify the branch this database
// writes to; seeding from it could fork this instance off another agent's
// lineage. The recorded owner is the only branch known to be this database's.
//
// Falls back to the agent branch itself whenever no usable record exists — none
// stored, it names the missing branch itself, or the branch it names is gone —
// so CreateBranch fails loudly rather than silently seeding from a broken
// source. Recovery from there is git's job: origin is the source of truth, and
// re-cloning rebuilds the database.
func (b *repoBuilder) seedSourceForAgentBranch() string {
	ctx := context.Background()
	if _, err := b.svc.Branches().HeadCommit(ctx, b.agentBranch); err == nil {
		return b.agentBranch // present: CreateBranch no-ops, source unused
	}
	owner, err := b.svc.Branches().AgentBranchOwner(ctx)
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).
			Msg("could not read the recorded agent branch owner; not taking over")
		return b.agentBranch
	}
	if owner == "" || owner == b.agentBranch {
		return b.agentBranch
	}
	if _, err := b.svc.Branches().HeadCommit(ctx, owner); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Str("recorded_owner", owner).
			Msg("recorded agent branch owner no longer exists; not taking over")
		return b.agentBranch
	}
	log.Info().Str("repo", b.name).Str("agent_branch", b.agentBranch).Str("from", owner).
		Msg("agent branch absent; taking over the repo from the recorded owner")
	return owner
}

// recordAgentBranchOwner claims this repo database for this instance's agent
// branch. Called only once CreateBranch has reported success, so the recorded
// owner always names a branch that exists.
//
// On a database predating the record this simply stamps the branch already in
// use, which is what closes the window: a later key regeneration then has a
// recorded owner to take over from instead of nothing.
func (b *repoBuilder) recordAgentBranchOwner(ctx context.Context) {
	if owner, err := b.svc.Branches().AgentBranchOwner(ctx); err == nil && owner == b.agentBranch {
		return
	}
	if err := b.svc.Branches().SetAgentBranchOwner(ctx, b.agentBranch); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Str("branch", b.agentBranch).
			Msg("could not record the agent branch owner; a later takeover would have nothing to seed from")
	}
}

// setupIndex configures the search index with the embedder and runs an initial
// sync against the git store. When the repo has an origin, the upstream branch
// is also synced — InitFromRemote populates commit_log for both agent/* and the
// upstream, but without an explicit index sync the upstream's branch_facts /
// facts_vec / graph tables would be empty even though the tree at HEAD has
// content cloned from origin. Without this, Verify's facts-coherence check
// correctly fires on the upstream branch whenever the cloned tree has any facts.
func (b *repoBuilder) setupIndex() {
	if b.embedder != nil {
		b.svc.SetEmbedder(b.embedder)
	}

	// Collect the branches whose index we maintain at startup. upstreamMain is
	// non-empty exactly when this repo has a stored origin (rehydrateUpstreamMain
	// reads it back from the remotes row), so it doubles as the has-origin test —
	// no config lookup, and no "main" guess for a master-convention repo.
	names := []string{b.agentBranch}
	if b.upstreamMain != "" {
		names = append(names, b.upstreamMain)
	}
	names = b.dropUnresolvableBranches(names)

	// If derived state was written by an older schema version (e.g. pre-canonical
	// domains / empty fact_domain_tokens), a plain Sync no-ops when last==HEAD and
	// leaves domain search silently broken. Ask PER BRANCH and full-Rebuild the
	// ones that are behind, which regenerates their derived state. For a
	// schema-version heal Rebuild preserves facts rowids, so existing embeddings
	// are reused and the corpus is not re-embedded. NeedsRebuild ALSO trips on an
	// embedding-identity change (model id / dim); in that case Rebuild's
	// ensureFactsVec recreates facts_vec empty and the corpus IS re-embedded under
	// the new model.
	//
	// Per branch, not once for the repo: the version is keyed per branch precisely
	// so a branch that could not be rebuilt last boot is the only one retried this
	// boot. Asking once and applying the answer to everything is what used to make
	// one permanently-failing branch re-index the whole repo on every startup.
	branches := make([]healBranch, 0, len(names))
	for _, name := range names {
		stale, err := b.svc.IndexManager().NeedsRebuild(context.Background(), name)
		if err != nil {
			log.Warn().Err(err).Str("repo", b.name).Str("branch", name).
				Msg("index schema version check failed; assuming current")
		}
		branches = append(branches, healBranch{name: name, stale: stale})
	}

	// Record the work; the heavy heal runs in the background after build() so
	// the server/UI come up immediately and reads work progressively. See
	// Manager.openOne.
	b.indexBranches = branches
}

// dropUnresolvableBranches removes branches whose git ref does not resolve, so
// the heal never schedules work that cannot succeed.
//
// The case that matters is a stored upstream naming a branch this repo does not
// have: a repo created locally (InitRepo makes the agent branch off "main")
// whose origin was later attached with a master-convention default, say. Rebuild
// fails at HeadCommit for such a branch on every boot, forever — it is a
// configuration disagreement between the remotes row and the repository, not a
// transient index problem, and reporting it as a rebuild failure buries that.
//
// Skipping is safe: a ref that does not resolve has no tree, so there is nothing
// for the index to hold. If the reconcile loop later creates the ref, its
// notifyCommit syncs the index for it in the normal way.
//
// The agent branch is never dropped. ensureBranch created it moments ago, and if
// it somehow did not resolve, a failing Rebuild is the right way to surface that
// — it is the branch local reads depend on, so the repo must report index-failed
// rather than quietly index nothing.
func (b *repoBuilder) dropUnresolvableBranches(names []string) []string {
	out := make([]string, 0, len(names))
	for i, name := range names {
		if i > 0 {
			if _, err := b.svc.Branches().HeadCommit(context.Background(), name); err != nil {
				log.Warn().Err(err).Str("repo", b.name).Str("branch", name).
					Msg("stored upstream branch does not exist in this repo; skipping its index heal " +
						"(check the origin's default branch against the remotes row)")
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

// healBranch is one unit of startup index work: a branch, plus whether ITS
// persisted schema version is behind (setupIndex asked NeedsRebuild per branch).
type healBranch struct {
	name  string
	stale bool
}

// healIndexBranches brings each maintained branch's search index up to date at
// startup: a branch whose own schema version is behind is full-Rebuilt to
// regenerate its derived state; the rest are incrementally Synced.
//
// It returns ok=false when the heal did not fully complete, so the caller can
// surface an index "error" state instead of falsely reporting "ready". Only the
// agent branch (index 0, the one local reads depend on) is fatal, on BOTH paths.
// An upstream-only failure (index > 0) is logged but NOT fatal: the local index
// is usable and the running reconcile loop owns upstream convergence, so
// flagging "error" there would stick on a transient remote hiccup.
//
// Nothing here re-arms a retry, and that is the point: Rebuild owns its own
// re-arm. The schema version is keyed per branch
// (meta.graph_schema_version:<branch>) and Rebuild drops that key when it fails,
// so a failed branch reports stale again next boot, alone. The version used to
// be global, which forced a choice between two broken options: let a healthy
// branch's bump mask the failed branch forever, or clear the global key and
// re-index EVERY branch on every boot for as long as the failure lasts —
// unbounded, since the common causes (a stored upstream naming a ref that does
// not exist locally) do not heal themselves.
func healIndexBranches(ctx context.Context, im store.IndexManager, repo string, branches []healBranch, progress store.RebuildProgress) (ok bool) {
	healFailed := false
	for i, branch := range branches {
		if branch.stale {
			if err := im.Rebuild(ctx, branch.name, progress); err != nil {
				level := log.Warn().Err(err).Str("repo", repo).Str("branch", branch.name)
				if i == 0 {
					level.Msg("schema-mismatch rebuild failed")
					healFailed = true
				} else {
					level.Msg("schema-mismatch rebuild (upstream) failed; the next startup retries this branch")
				}
			}
			continue
		}
		// SyncLocked, not Sync: this heal runs in the background while the store
		// is live, so it must hold lockBranch to stay mutually exclusive with a
		// concurrent inline write's notifyCommit sync or the commit observer.
		if err := im.SyncLocked(ctx, branch.name); err != nil {
			level := log.Warn().Err(err).Str("repo", repo).Str("branch", branch.name)
			if i == 0 {
				level.Msg("initial index sync failed")
				healFailed = true
			} else {
				level.Msg("initial index sync (upstream) failed; reconcile loop will retry")
			}
		}
	}
	return !healFailed
}

// seedWatermarks sets the pipeline watermark to HEAD for any tool that has no
// watermark for the current agent branch, so the first pipeline run only
// processes facts written after this point.
func (b *repoBuilder) seedWatermarks() {
	for _, tool := range []string{"review", "hypothesize"} {
		if wm, _ := b.svc.Pipeline().GetPipelineWatermark(context.Background(), tool, b.agentBranch); wm == "" {
			if head, err := b.svc.Branches().HeadCommit(context.Background(), b.agentBranch); err == nil {
				if err := b.svc.Pipeline().SetPipelineWatermark(context.Background(), tool, b.agentBranch, head); err != nil {
					log.Warn().Err(err).Str("tool", tool).Msg("pipeline watermark: initial set failed")
				}
			}
		}
	}
}

// build assembles the final RepoInstance, starts the commit observer and
// background sync loops, and wires up the startSync and closeFn closures.
// Must be called after openStore, openGit, ensureBranch, setupIndex, and
// seedWatermarks. The returned instance is ready for registration with the Manager.
func (b *repoBuilder) build() *RepoInstance {
	hub := NewTaskHub(b.ctx)

	// Allocate ri first — the observer and closures capture the pointer so
	// they follow SwapStore field replacements via the read lock.
	ri := &RepoInstance{
		uid:                           b.uid,
		dbPath:                        b.dbPath,
		agentBranch:                   b.agentBranch,
		ontology:                      b.ontology,
		embedder:                      b.embedder,
		ontologyRoot:                  b.cfg.OntologyRoot,
		methodologyMinScore:           b.cfg.MethodologyMinScore,
		clusterResolution:             clusterResolutionOrDefault(b.cfg.ClusterCache.Resolution),
		clusterMinCommunity:           clusterMinCommunityOrDefault(b.cfg.ClusterCache.MinCommunitySize),
		discoveryEffortDefault:        b.cfg.Discovery.EffortDefault,
		discoveryConfidenceThreshold:  b.cfg.Discovery.ConfidenceThreshold,
		discoveryBlastRadiusThreshold: b.cfg.Discovery.BlastRadiusThreshold,
		discoveryBridge:               b.cfg.Discovery.Bridge,
		discoveryCohFloor:             b.cfg.Discovery.CohFloor,
		discoveryMaxMembers:           b.cfg.Discovery.MaxMembers,
		discoveryQualityFloor:         b.cfg.Discovery.QualityFloor,
		discoveryWCoh:                 b.cfg.Discovery.WCoh,
		discoveryWGap:                 b.cfg.Discovery.WGap,
		discoveryWSpec:                b.cfg.Discovery.WSpec,
		handle:                        newStoreHandle(b.svc),
		hub:                           hub,
	}
	ri.setName(b.name)

	// Observer: sync index + push SSE on every git commit.
	//
	// SyncLocked (not bare Sync): the observer can fire while the background
	// heal is still rebuilding this branch, so it must hold lockBranch to
	// serialize with that Rebuild and with inline writes. Bare Sync here would
	// observe the heal's cleared watermark and race a full re-index.
	obs := newCommitObserver(time.Second, func(hash string) {
		// Acquire (not a bare pointer snapshot) so a teardown/SwapStore that
		// fires while this callback is mid-Sync waits for the release below
		// instead of closing the SQLite handle under the running sync. Once
		// teardown has begun the Acquire fails and the sync is skipped — the
		// store is going away, so there is nothing left worth indexing.
		currentSvc, release, err := ri.Acquire()
		if err != nil {
			return
		}
		defer release()
		if err := currentSvc.IndexManager().SyncLocked(context.Background(), b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Msg("observer sync failed")
		}
		hub.broadcastStatus(hash)
	})
	ri.onCommit = func(_, hash string) { obs.Notify(hash) }
	b.svc.SetOnCommit(ri.onCommit)

	// Startup recovery + the remote sync loops are DEFERRED to activate(), run
	// by Manager.openOne AFTER the background index completes — so the reconcile
	// loop never races the initial index build. The commit observer above IS
	// wired now (live during the background heal), but it is safe: its index
	// mutation goes through SyncLocked, which serializes with the heal's
	// lockBranch-holding Rebuild. We create the sync context now so the
	// startSync closure and shutdown's cancel work.
	syncCtx, syncCancel := context.WithCancel(b.ctx)
	var syncWg sync.WaitGroup

	ri.syncCancel = syncCancel
	ri.syncWg = &syncWg
	b.hub = hub
	b.syncCtx = syncCtx
	b.syncWg = &syncWg

	// The background index heal gets its OWN context + waitgroup, distinct from
	// syncCtx/syncWg. startSync (ActivateSync) cancels syncCtx to restart the
	// reconcile loop; if the heal shared that context, a runtime clone-create —
	// which calls ActivateSync immediately after openOne launches the heal —
	// would cancel the in-flight initial index, leaving it stuck "indexing"
	// forever. indexCtx is cancelled only by a real teardown (shutdown /
	// Manager.Close / SwapStore), each of which also waits indexWg before
	// svc.Close(). Derived from b.ctx so Manager-context cancellation reaches it.
	indexCtx, indexCancel := context.WithCancel(b.ctx)
	var indexWg sync.WaitGroup
	ri.indexCancel = indexCancel
	ri.indexWg = &indexWg
	b.indexCtx = indexCtx
	b.indexWg = &indexWg

	// Wire closures that capture ri so they follow SwapStore replacements.
	cfg := b.cfg
	keyPath := b.keyPath
	ctx := b.ctx
	name := b.name
	agentBranch := b.agentBranch
	noBackgroundSync := b.disableBackgroundSync

	ri.startSync = func(remoteURL string) error {
		// Hold a store reference for the whole activation (remote read +
		// synchronous reconcile) so a concurrent SwapStore/teardown drains this
		// call rather than closing the service under it. The reconcile loop
		// started at the end captures currentSvc beyond this scope; that is
		// safe because every close/swap path cancels the loop and waits syncWg
		// before touching the store.
		currentSvc, release, err := ri.Acquire()
		if err != nil {
			return fmt.Errorf("ActivateSync: %w", err)
		}
		defer release()

		remote, err := currentSvc.Remote().GetRemote("origin")
		if err != nil || remote == nil {
			return fmt.Errorf("read remote: %w", err)
		}

		// Re-assert the local-origin policy before touching anything, the same
		// way each background tick does (runReconcileLoop). A stored origin that
		// no longer satisfies the policy must not be fetched — and rejecting it
		// here, before the teardown below, leaves any currently-running loop
		// intact rather than killing it and orphaning ri.syncCancel.
		if verr := validateLocalOrigin(remote.URL, cfg.LocalOriginRoot); verr != nil {
			return fmt.Errorf("ActivateSync: origin blocked by local-origin policy: %w", verr)
		}

		syncCancel()
		syncWg.Wait()

		var newCtx context.Context
		newCtx, syncCancel = context.WithCancel(ctx)
		ri.mu.Lock()
		ri.syncCancel = syncCancel
		ri.mu.Unlock()

		currentSvc.SetOnCommit(ri.onCommit)

		// One synchronous reconcile so the migration / token-refresh happens
		// on this call. Fail-fast: if the reconcile errors, return the
		// error to the HAL handler so the HTTP response surfaces a bad
		// token (or unreachable origin) immediately. The loops are NOT
		// started on failure — the user must retry the origin PUT (typically
		// with a corrected token).
		//
		// Rationale: this endpoint exists primarily to (a) configure
		// origin for the first time, and (b) refresh an expired token.
		// In both cases, immediate feedback on bad credentials is worth
		// far more than tolerating transient network blips (which the
		// user can recover from by retrying the origin PUT with the same
		// token).
		//
		// Build the auth factory once and reuse it for the synchronous
		// reconcile and the loops. Using the factory (instead of the
		// static cfg.Remote) ensures we resolve auth from the DB-stored
		// remote record — so a token just refreshed via PUT
		// /api/v1/{repo}/origin is honoured immediately, and SSH URLs
		// are auto-detected via resolveAuthWithOrigin.
		authFn := makeRemoteAuthFn(cfg.Remote, keyPath)
		auth, authErr := authFn(remote)
		if authErr != nil {
			// Auth resolution failed (unreadable key / malformed credential).
			// Surface it to the HAL handler and record it on the remote so the
			// caller sees the misconfiguration, rather than syncing anonymously.
			if serr := currentSvc.Remote().RecordSyncError("origin", authErr.Error()); serr != nil {
				log.Warn().Err(serr).Str("repo", name).Msg("ActivateSync: failed to persist auth-resolution error")
			}
			return fmt.Errorf("ActivateSync: auth resolution failed: %w", authErr)
		}
		if _, err := currentSvc.Remote().Sync(newCtx, agentBranch, auth); err != nil {
			return fmt.Errorf("ActivateSync: initial reconcile failed: %w", err)
		}

		// Honour DisableBackgroundSync here too, exactly as startSyncLoops does.
		// The synchronous reconcile above is the caller's explicit request and
		// still runs; it is the periodic loop that a harness setting this flag
		// is asking not to have racing its assertions. Without this, any path
		// that activates sync (repo create in clone mode, PUT .../origin) would
		// smuggle a background loop past the flag.
		if noBackgroundSync {
			return nil
		}
		syncWg.Add(1)
		go runReconcileLoop(newCtx, &syncWg, currentSvc, hub, name, agentBranch, authFn, cfg.LocalOriginRoot, cfg.ReadOnly)
		return nil
	}

	ri.closeFn = func() {
		obs.Stop()
		// Detach the handle (marking the instance closed so any later Acquire —
		// e.g. an HTTP handler that resolved this ri before Archive removed it
		// from the map, or an observer callback that fires after Stop's flush —
		// fails with ErrRepoClosed), then drain every in-flight user before
		// closing the SQLite handle. Draining on the refcount, not the lock, is
		// what covers users that acquired earlier and are still mid-operation.
		h := ri.detachStore(true)
		if h == nil {
			return
		}
		h.wg.Wait()
		h.svc.Close()
	}

	return ri
}

// activate runs the deferred startup reconcile and starts the background sync +
// push loops. Manager.openOne calls it AFTER the background index completes, so
// the reconcile loop never races the initial index build. (The commit observer
// is wired earlier in build() but is race-safe via SyncLocked; only the remote
// reconcile/push loops are deferred here.)
func (b *repoBuilder) activate() {
	b.recoverFromOrigin()
	b.startSyncLoops(b.syncCtx, b.syncWg, b.hub)
}

// recoverFromOriginTimeout bounds the startup reconcile so a slow or
// unreachable origin cannot stall repo construction past this duration.
// The background loop retries on its own cadence; failing fast here keeps
// boot snappy and surfaces auth/network issues quickly without blocking.
const recoverFromOriginTimeout = 15 * time.Second

// recoverFromOrigin runs one reconcile cycle on startup for a repo that has a
// stored origin. Failures are logged but non-fatal — the sync loops will
// retry on their next tick. This catches the reinstall-with-state-intact
// case (we have local state but need to resume against origin) and the
// token-expired-then-fixed case (auth used to fail, has been updated,
// next iteration succeeds). Skipped when DisableBackgroundSync is set so
// test harnesses don't hit a non-existent origin at construction time.
//
// The stored remote row is the only origin test: a repo has an origin iff it
// has one persisted, which is also the record the reconcile below reads.
func (b *repoBuilder) recoverFromOrigin() {
	if b.disableBackgroundSync {
		return
	}
	remote, err := b.svc.Remote().GetRemote("origin")
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).
			Msg("recoverFromOrigin: read remote failed; skipping startup reconcile (loop will retry on next tick)")
		return
	}
	if remote == nil {
		return
	}
	// Apply the local-origin policy on the startup reconcile too, matching the
	// loop's per-tick gate. Without this, a stored local origin that the current
	// policy forbids would still be fetched once at boot.
	if verr := validateLocalOrigin(remote.URL, b.cfg.LocalOriginRoot); verr != nil {
		log.Error().Err(verr).Str("repo", b.name).Msg("recoverFromOrigin: origin blocked by local-origin policy; skipping startup reconcile")
		return
	}
	// Use the same factory the loops use so we pick up any fresh token /
	// auth config stored in the DB (e.g. after a PUT /api/v1/{repo}/origin
	// refresh) instead of the static b.cfg.Remote captured at startup.
	authFn := makeRemoteAuthFn(b.cfg.Remote, b.keyPath)
	auth, authErr := authFn(remote)
	if authErr != nil {
		// Auth resolution failed at startup. Record the error on the remote so
		// it is visible (the background loop will retry on its next tick) rather
		// than silently syncing anonymously.
		if serr := b.svc.Remote().RecordSyncError("origin", authErr.Error()); serr != nil {
			log.Warn().Err(serr).Str("repo", b.name).Msg("recoverFromOrigin: failed to persist auth-resolution error")
		}
		log.Warn().Err(authErr).Str("repo", b.name).Msg("recoverFromOrigin: auth resolution failed (will retry in loop)")
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, recoverFromOriginTimeout)
	defer cancel()
	if _, err := b.svc.Remote().Sync(ctx, b.agentBranch, auth); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("recoverFromOrigin: initial sync failed (will retry in loop)")
	}
}

// startSyncLoops launches the background pull and push goroutines if a remote
// named "origin" is configured. Skipped entirely when Deps.DisableBackgroundSync
// is set — test harnesses use that flag to prevent the first-tick immediate
// doSync/doPush call from racing with test assertions.
func (b *repoBuilder) startSyncLoops(ctx context.Context, wg *sync.WaitGroup, hub *TaskHub) {
	if b.disableBackgroundSync {
		return
	}
	remote, err := b.svc.Remote().GetRemote("origin")
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).
			Msg("startSyncLoops: read remote failed; reconcile loop not started for this repo")
		return
	}
	if remote == nil {
		return
	}

	authFn := makeRemoteAuthFn(b.cfg.Remote, b.keyPath)
	wg.Add(1)
	go runReconcileLoop(ctx, wg, b.svc, hub, b.name, b.agentBranch, authFn, b.cfg.LocalOriginRoot, b.cfg.ReadOnly)
}

// close releases resources opened so far. Safe to call at any point during
// the build sequence before build() — nil fields are skipped.
func (b *repoBuilder) close() {
	if b.svc != nil {
		b.svc.Close()
	}
}
