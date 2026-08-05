package repos

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	dbPath                string
	cfg                   config.Config
	signer                ssh.Signer
	agentBranch           string
	embedder              store.BatchEmbedder
	keyPath               string
	ctx                   context.Context
	disableBackgroundSync bool
	// authResolve resolves this repo's origin credential from control.db,
	// fresh at each call. It is Manager.OriginAuth bound to this repo's name by
	// openOne. All three call sites (build's ri.startSync closure,
	// recoverFromOrigin, startSyncLoops) pass it straight through to
	// makeRemoteAuthFn UNRESOLVED — makeRemoteAuthFn calls it itself, inside
	// the remoteAuthFn it returns, so a resolution failure reaches that
	// function's own error path (recorded via RecordSyncError) instead of
	// being caught and downgraded to a credential-less sync here. See
	// makeRemoteAuthFn's doc comment for why resolving eagerly at any of
	// these call sites is the bug this shape exists to rule out.
	//
	// A repoBuilder does not hold a *Manager back-pointer, so this is threaded
	// in as a function value instead of storing the config once at
	// construction time: the recoverFromOrigin/startSyncLoops call sites fire
	// once at build time either way, but the third call site — inside
	// ri.startSync (ActivateSync), used by PUT /repos/{repo}/origin to reconcile
	// immediately after a token refresh — is invoked long after construction,
	// on the SAME long-lived closure, for the life of the RepoInstance. A value
	// captured once at openOne would go stale the moment a credential changed
	// without a full repo reopen, silently breaking the "refreshed token is
	// honoured immediately" contract that closure's own comment documents.
	authResolve func() (config.RemoteAuthConfig, error)

	// accumulated state
	svc      *store.Service
	ontology *fact.Ontology

	// index work deferred to the background (set by setupIndex, run after
	// build): the branches whose index we maintain and whether a schema-version
	// mismatch forces a full rebuild.
	indexBranches []string
	indexStale    bool

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
	// upstreamMain is this repo's consensus branch name (e.g. "main" or
	// "master"), read back from its own stored origin remote by
	// resolveUpstreamMain. Empty means the repo has no origin, which is also
	// what tells setupIndex there is no upstream branch to index.
	upstreamMain string
}

// openStore opens the SQLite-backed store and configures credential encryption.
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
	// Without a Crypt, SetRemote REFUSES to persist any auth token (never
	// plaintext); configureCrypt logs a warning so that refusal is observable.
	configureCrypt(svc, b.keyPath, b.name)
	return nil
}

// configureCrypt wires credential encryption onto svc from the agent key at
// keyPath. On any failure the store keeps no Crypt, so SetRemote will REFUSE
// to persist auth tokens (never plaintext); the warning makes that refusal
// observable rather than silent. repo labels the log line for diagnosis.
func configureCrypt(svc *store.Service, keyPath, repo string) {
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		log.Warn().Err(err).Str("repo", repo).Str("key_path", keyPath).
			Msg("credential encryption unavailable: agent key unreadable; auth tokens cannot be stored")
		return
	}
	crypt, err := store.NewCrypt(keyData)
	if err != nil {
		log.Warn().Err(err).Str("repo", repo).
			Msg("credential encryption unavailable: cannot derive key; auth tokens cannot be stored")
		return
	}
	svc.SetCrypt(crypt)
}

// openGit opens the git repository backed by the store.
//
// It never initialises one. Every repo is created explicitly — Manager.Create
// runs InitRepo (preset/custom) or InitFromRemote (clone) BEFORE the instance
// is opened — so a database that reaches here without git data is a genuine
// fault, not a first run. There is no default repo to bootstrap on the side.
func (b *repoBuilder) openGit() error {
	if err := b.svc.OpenRepo(); err != nil {
		return fmt.Errorf("open git: %w", err)
	}
	b.resolveUpstreamMain()
	b.svc.SetSigner(b.signer)
	return nil
}

// resolveUpstreamMain reads this repo's consensus branch back off its own
// stored origin remote, leaving it empty when the repo has no origin.
//
// The record is the only per-repo source for it. Guessing "main" instead
// silently rewrote the persisted upstream and the fetch refspec on the next
// boot of any repo tracking something else (e.g. master), and aimed the startup
// index sync at a branch that does not exist. Mirrors recoverFromOrigin, which
// reads GetRemote for the same reason.
func (b *repoBuilder) resolveUpstreamMain() {
	remote, err := b.svc.Remote().GetRemote("origin")
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).
			Msg("read stored remote failed; repo treated as having no upstream branch")
		return
	}
	if remote == nil || remote.URL == "" {
		return // no origin: nothing upstream to track
	}
	b.upstreamMain = remote.Branch
	if b.upstreamMain == "" {
		b.upstreamMain = "main"
	}
	b.warnIfUpstreamMissing()
}

// warnIfUpstreamMissing reports a stored upstream that names a branch this
// database does not have.
//
// InitFromRemote ALWAYS creates the local upstream branch, so the two can only
// disagree if something wrote the record without bootstrapping the branch. The
// clone path did exactly that until the prefer-main fix: it resolved (say)
// "master" for the clone and then persisted "main", leaving the record, the
// fetch refspec and the local refs describing three different things. Repos
// created that way still carry the wrong value, and nothing re-detects it —
// this instance faithfully reads it back.
//
// Deliberately a loud diagnostic and NOT a self-heal. Re-detecting from the
// remote at every boot would cost a network round-trip per repo and could
// silently overrule a deliberate choice; guessing from the local refs would be
// a heuristic on data we already know is inconsistent. Name the repair instead
// — SetUpstreamBranch is the primitive built for it, reachable over HTTP.
func (b *repoBuilder) warnIfUpstreamMissing() {
	if _, err := b.svc.Branches().HeadCommit(context.Background(), b.upstreamMain); err == nil {
		return
	}
	log.Error().Str("repo", b.name).Str("upstream", b.upstreamMain).
		Msgf("stored upstream branch %q does not exist in this repo; sync will not converge. "+
			"If this repo was cloned from a remote whose default branch is not %q, its recorded "+
			"upstream is wrong — repoint it with: PUT /api/v1/repos/%s/origin {\"branch\": \"<real-branch>\"}",
			b.upstreamMain, b.upstreamMain, b.name)
}

// loadOntology reads the ontology from the repo's agent branch, preferring
// OntologyPath and falling back to LegacyOntologyPath for repos that predate
// the private-directory move (no migration is provided). Falls back to the
// default ontology if neither file is present or the content is unparseable.
func (b *repoBuilder) loadOntology() {
	if b.svc == nil {
		b.ontology = fact.DefaultOntology()
		return
	}
	// Track the source path: the refresh below writes BACK to it. Always
	// writing the canonical path would leave a legacy repo holding two
	// ontology files, the stale one indistinguishable from the live one.
	srcPath := OntologyPath
	result, err := b.svc.Facts().ReadFact(context.Background(), b.agentBranch, OntologyPath, nil)
	if err != nil || result.Content == "" {
		srcPath = LegacyOntologyPath
		result, err = b.svc.Facts().ReadFact(context.Background(), b.agentBranch, LegacyOntologyPath, nil)
	}
	if err != nil || result.Content == "" {
		log.Warn().Str("repo", b.name).
			Msgf("no ontology at %s or %s, using default ontology", OntologyPath, LegacyOntologyPath)
		b.ontology = fact.DefaultOntology()
		return
	}
	if srcPath == LegacyOntologyPath {
		log.Info().Str("repo", b.name).
			Msgf("ontology loaded from the legacy path %s; rename it to %s", LegacyOntologyPath, OntologyPath)
	}
	ont, err := fact.ParseOntology([]byte(result.Content))
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
// sync against the git store. When THIS repo has an origin, the upstream
// branch is also synced — InitFromRemote populates commit_log for both
// agent/* and the upstream, but without an explicit index sync the upstream's
// branch_facts / facts_vec / graph tables would be empty even though the
// tree at HEAD has content cloned from origin. Without this, Verify's
// facts-coherence check correctly fires on the upstream branch whenever the
// cloned tree has any facts.
func (b *repoBuilder) setupIndex() {
	if b.embedder != nil {
		b.svc.SetEmbedder(b.embedder)
	}

	// Collect the branches whose index we maintain at startup. upstreamMain is
	// non-empty exactly when this repo has its own stored origin, so the
	// upstream is indexed per repo rather than off a server-wide setting.
	branches := []string{b.agentBranch}
	if b.upstreamMain != "" {
		branches = append(branches, b.upstreamMain)
	}

	// If derived state was written by an older schema version (e.g. pre-canonical
	// domains / empty fact_domain_tokens), a plain Sync no-ops when last==HEAD and
	// leaves domain search silently broken. Detect the mismatch once (the version
	// is global) and full-Rebuild each branch instead, which regenerates the
	// derived state. For a schema-version heal Rebuild preserves facts rowids, so
	// existing embeddings are reused and the corpus is not re-embedded. NeedsRebuild
	// ALSO trips on an embedding-identity change (model id / dim); in that case
	// Rebuild's ensureFactsVec recreates facts_vec empty and the corpus IS
	// re-embedded under the new model.
	stale, err := b.svc.IndexManager().NeedsRebuild(context.Background())
	if err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("index schema version check failed; assuming current")
	}

	// Record the work; the heavy heal runs in the background after build() so
	// the server/UI come up immediately and reads work progressively. See
	// Manager.openOne.
	b.indexBranches = branches
	b.indexStale = stale
}

// healIndexBranches brings each maintained branch's search index up to date at
// startup: when `stale` (the global schema version is behind), it full-Rebuilds
// every branch to regenerate derived state; otherwise it incrementally Syncs.
//
// It returns ok=false when the heal did not fully complete, so the caller can
// surface an index "error" state instead of falsely reporting "ready". A failed
// rebuild of any branch, or a failed initial Sync of the agent branch (index 0,
// the one local reads depend on), counts as a failure. An upstream-only Sync
// failure (index > 0) is logged but NOT fatal: the local index is usable and
// the running reconcile loop owns upstream convergence, so flagging "error"
// there would stick on a transient remote hiccup.
//
// Rebuild bumps the GLOBAL meta.graph_schema_version on each branch it
// completes. So if an earlier branch rebuilds successfully and a later branch
// fails, the version already reads current and the next startup would skip the
// heal entirely — leaving the failed branch's canonical domains / tokens stale
// permanently. To prevent that, any rebuild failure during a heal re-marks the
// schema as needing a rebuild so the next startup retries every branch.
func healIndexBranches(ctx context.Context, im store.IndexManager, repo string, branches []string, stale bool, progress store.RebuildProgress) (ok bool) {
	healFailed := false
	for i, branch := range branches {
		if stale {
			if err := im.Rebuild(ctx, branch, progress); err != nil {
				log.Warn().Err(err).Str("repo", repo).Str("branch", branch).Msg("schema-mismatch rebuild failed")
				healFailed = true
			}
			continue
		}
		// SyncLocked, not Sync: this heal runs in the background while the store
		// is live, so it must hold lockBranch to stay mutually exclusive with a
		// concurrent inline write's notifyCommit sync or the commit observer.
		if err := im.SyncLocked(ctx, branch); err != nil {
			level := log.Warn()
			if i == 0 {
				level.Err(err).Str("repo", repo).Msg("initial index sync failed")
				healFailed = true
			} else {
				level.Err(err).Str("repo", repo).Str("branch", branch).Msg("initial index sync (upstream) failed; reconcile loop will retry")
			}
		}
	}
	if stale && healFailed {
		if err := im.MarkRebuildNeeded(ctx); err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("could not re-mark schema rebuild after partial heal")
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
		name:                          b.name,
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
	authResolve := b.authResolve

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
		// started on failure — the user must retry SetRemote (typically
		// with a corrected token).
		//
		// Rationale: this endpoint exists primarily to (a) configure
		// origin for the first time, and (b) refresh an expired token.
		// In both cases, immediate feedback on bad credentials is worth
		// far more than tolerating transient network blips (which the
		// user can recover from by retrying SetRemote with the same
		// token).
		//
		// Build the auth factory once and reuse it for the synchronous
		// reconcile and the loops. authResolve reads control.db fresh on
		// every invocation of the returned authFn (not just once here) — so
		// a token just refreshed via PUT /api/v1/{repo}/origin is honoured
		// immediately, and SSH URLs are auto-detected via
		// resolveAuthWithOrigin. Passed straight through, unresolved: a
		// resolution failure must reach authFn's own error path (below) and
		// be recorded, not be swallowed here into a credential-less fallback.
		authFn := makeRemoteAuthFn(authResolve, keyPath)
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

		// DisableBackgroundSync means what it says on EVERY loop-starting path,
		// not just startSyncLoops. runReconcileLoop opens with an immediate tick
		// that fetches AND pushes, so a harness that asked for no background sync
		// would otherwise get a live remote conversation racing its assertions the
		// moment anything called ActivateSync (Manager.Create in clone mode does).
		// The synchronous reconcile above still runs: that is the fail-fast
		// credential check this call exists for, and it does not push.
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

// recoverFromOrigin runs one reconcile cycle on startup if THIS repo has an
// origin stored. Failures are logged but non-fatal — the sync loops will
// retry on their next tick. This catches the reinstall-with-state-intact
// case (we have local state but need to resume against origin) and the
// token-expired-then-fixed case (auth used to fail, has been updated,
// next iteration succeeds). Skipped when DisableBackgroundSync is set so
// test harnesses don't hit a non-existent origin at construction time.
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
	if remote == nil || remote.URL == "" {
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
	// auth config stored in control.db (e.g. after a PUT /api/v1/{repo}/origin
	// refresh) instead of the static b.cfg.Remote captured at startup.
	// Resolution happens inside authFn itself (see makeRemoteAuthFn) — a
	// resolve failure must reach the authErr branch below and be recorded,
	// not be swallowed here into a credential-less fallback.
	authFn := makeRemoteAuthFn(b.authResolve, b.keyPath)
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

	// Resolution happens inside authFn itself on every reconcile tick (see
	// makeRemoteAuthFn) — not eagerly here, so a resolve failure reaches the
	// tick's own error/RecordSyncError path instead of being swallowed into a
	// credential-less fallback.
	authFn := makeRemoteAuthFn(b.authResolve, b.keyPath)
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
