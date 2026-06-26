package repos

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/observe"
	"knomit/internal/store"
)

// repoBuilder accumulates state while constructing a RepoInstance step by step.
// Each method returns an error; the caller (Manager.openOne) checks after each
// step and can clean up with close() on failure.
type repoBuilder struct {
	// inputs
	name                  string
	dbPath                string
	isDefault             bool
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
	// upstreamMain is the resolved consensus branch name for this repo's
	// origin (e.g. "main" or "master"). Populated by initDefaultGit when
	// origin is configured (detected from the remote's symbolic HEAD).
	// Defaults to "main" for repos with no origin.
	upstreamMain string
}

// openStore opens the SQLite-backed store and configures credential encryption.
func (b *repoBuilder) openStore() error {
	svc, err := store.Open(b.dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	b.svc = svc
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

// openGit opens or initialises the git repository backed by the store.
// For the default repo on first run it clones from origin or creates a fresh
// repo with the default ontology.
func (b *repoBuilder) openGit() error {
	err := b.svc.OpenRepo()
	if err != nil {
		if !b.isDefault {
			return fmt.Errorf("open git: %w", err)
		}
		if err = b.initDefaultGit(); err != nil {
			return err
		}
	}
	b.svc.SetSigner(b.signer)
	return nil
}

// loadOntology reads domains/ontology.yaml from the repo's agent branch.
// Falls back to the default ontology if the file is absent or unparseable.
func (b *repoBuilder) loadOntology() {
	if b.svc == nil {
		b.ontology = fact.DefaultOntology()
		return
	}
	result, err := b.svc.Facts().ReadFact(context.Background(), b.agentBranch, "domains/ontology.yaml", nil)
	if err != nil || result.Content == "" {
		log.Warn().Str("repo", b.name).Msg("domains/ontology.yaml not found, using default ontology")
		b.ontology = fact.DefaultOntology()
		return
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
					Msg("ontology refresh: stored is subset of embedded preset; upgrading to latest")
				if _, werr := b.svc.Facts().WriteFact(
					context.Background(),
					b.agentBranch,
					"domains/ontology.yaml",
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

// resolveOriginUpstream queries the configured origin's symbolic HEAD to
// determine the remote's default branch name (e.g. "main", "master").
// Returns "main" as the fallback when detection fails — but emits a warn
// log first so an operator can see that detection actually failed (rather
// than the remote genuinely defaulting to main).
//
// Detection failure typically means: bad auth (token wrong/expired),
// unreachable URL (DNS/network), or the remote has no symbolic HEAD set.
// In all three cases the operator needs a signal — silently picking "main"
// for a `master`-default repo creates a configuration mismatch that the
// user will only notice when origin/main forever appears empty.
func (b *repoBuilder) resolveOriginUpstream(auth transport.AuthMethod) string {
	upstream := store.DetectRemoteUpstreamFromURL(b.cfg.Git.Origin, auth)
	if upstream != "" {
		log.Info().Str("repo", b.name).Str("upstream", upstream).
			Msg("initDefaultGit: detected upstream branch from remote HEAD")
		return upstream
	}
	log.Warn().Str("repo", b.name).Str("origin", b.cfg.Git.Origin).
		Msg("initDefaultGit: could not detect remote HEAD; defaulting to \"main\" (check auth/connectivity if origin uses a different default)")
	return "main"
}

// initDefaultGit creates the git store for the default ("trunk") repo on
// first run — either by cloning from a configured origin or by creating a
// fresh repository with the default ontology seed files.
func (b *repoBuilder) initDefaultGit() error {
	ont := fact.DefaultOntology()
	ontologyYAML, err := ont.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}
	seedFiles := map[string]string{
		"domains/ontology.yaml": string(ontologyYAML),
	}

	if b.cfg.Git.Origin != "" {
		// The local-origin policy is absolute and applies to the operator's own
		// config origin too: a filesystem origin is permitted only inside
		// LocalOriginRoot, and when that root is unset, filesystem origins are
		// unavailable everywhere — including here. Reject before any auth, ls-
		// remote, or fetch so a forbidden path is never touched. (Network origins
		// pass through untouched.) This matches the gate on every other path:
		// ResolveAuth, recoverFromOrigin, ActivateSync, and the sync loop.
		if verr := validateLocalOrigin(b.cfg.Git.Origin, b.cfg.LocalOriginRoot); verr != nil {
			return fmt.Errorf("initDefaultGit: origin blocked by local-origin policy: %w", verr)
		}
		auth, authErr := resolveAuth(b.cfg.Remote, b.keyPath)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		// Detect the remote's default branch via ls-remote so the local
		// refspecs and SetRemote record line up with it. Capture the
		// resolved value on the builder so ensureBranch can pass it to
		// SetRemote. A failed detection here is logged at warn level —
		// see resolveOriginUpstream's comment for why this matters.
		b.upstreamMain = b.resolveOriginUpstream(auth)
		if err := b.svc.InitFromRemote(b.cfg.Git.Origin, auth, b.upstreamMain, b.agentBranch, seedFiles); err != nil {
			return fmt.Errorf("init from remote: %w", err)
		}
		return nil
	}

	if err := b.svc.InitRepo(seedFiles, b.agentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	return nil
}

// ensureBranch creates the agent branch if it doesn't already exist and seeds
// the origin remote record for the default repo.
func (b *repoBuilder) ensureBranch() {
	if b.agentBranch != "" {
		if err := b.svc.Branches().CreateBranch(context.Background(), b.agentBranch, b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Msg("branch create/ensure failed")
		}
	}
	if b.isDefault && b.cfg.Git.Origin != "" {
		upstream := b.upstreamMain
		if upstream == "" {
			upstream = "main"
		}
		if err := b.svc.Remote().SetRemote("origin", b.cfg.Git.Origin, upstream, b.agentBranch, 300, 300, "", ""); err != nil {
			log.Warn().Err(err).Msg("failed to seed origin in remotes table")
		}
	}
}

// setupIndex configures the search index with the embedder and runs an initial
// sync against the git store. When an origin is configured, the upstream
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

	// Collect the branches whose index we maintain at startup.
	branches := []string{b.agentBranch}
	if b.cfg.Git.Origin != "" {
		upstream := b.upstreamMain
		if upstream == "" {
			upstream = "main"
		}
		branches = append(branches, upstream)
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
		name:                b.name,
		dbPath:              b.dbPath,
		agentBranch:         b.agentBranch,
		ontology:            b.ontology,
		embedder:            b.embedder,
		ontologyRoot:        b.cfg.OntologyRoot,
		methodologyMinScore: b.cfg.MethodologyMinScore,
		clusterResolution:   clusterResolutionOrDefault(b.cfg.ClusterCache.Resolution),
		clusterMinCommunity: clusterMinCommunityOrDefault(b.cfg.ClusterCache.MinCommunitySize),
		svc:                 b.svc,
		hub:                 hub,
	}

	// Observer: sync index + push SSE on every git commit.
	//
	// SyncLocked (not bare Sync): the observer can fire while the background
	// heal is still rebuilding this branch, so it must hold lockBranch to
	// serialize with that Rebuild and with inline writes. Bare Sync here would
	// observe the heal's cleared watermark and race a full re-index.
	obs := observe.New(time.Second, func(hash string) {
		ri.mu.RLock()
		currentSvc := ri.svc
		ri.mu.RUnlock()
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

	ri.startSync = func(remoteURL string) error {
		ri.mu.RLock()
		currentSvc := ri.svc
		ri.mu.RUnlock()

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
		// reconcile and the loops. Using the factory (instead of the
		// static cfg.Remote) ensures we resolve auth from the DB-stored
		// remote record — so a token just refreshed via PUT
		// /api/v1/{repo}/origin is honoured immediately, and SSH URLs
		// are auto-detected via resolveAuthWithOrigin.
		authFn := makeRemoteAuthFn(cfg.Remote, keyPath)
		auth := authFn(remote)
		if _, err := currentSvc.Remote().Sync(newCtx, agentBranch, auth); err != nil {
			return fmt.Errorf("ActivateSync: initial reconcile failed: %w", err)
		}

		syncWg.Add(1)
		go runReconcileLoop(newCtx, &syncWg, currentSvc, hub, name, agentBranch, authFn, cfg.LocalOriginRoot)
		return nil
	}

	ri.closeFn = func() {
		obs.Stop()
		// Acquire the WRITE lock to drain all in-flight readers (WithRead holds
		// the RLock while calling fn(svc)) before closing the SQLite handle.
		// Using RLock here would let a concurrent WithRead execute fn(svc)
		// while Close runs → use-after-close.
		ri.mu.Lock()
		svc := ri.svc
		ri.mu.Unlock()
		svc.Close()
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

// recoverFromOrigin runs one reconcile cycle on startup if origin is
// configured. Failures are logged but non-fatal — the sync loops will
// retry on their next tick. This catches the reinstall-with-state-intact
// case (we have local state but need to resume against origin) and the
// token-expired-then-fixed case (auth used to fail, has been updated,
// next iteration succeeds). Skipped when DisableBackgroundSync is set so
// test harnesses don't hit a non-existent origin at construction time.
func (b *repoBuilder) recoverFromOrigin() {
	if b.disableBackgroundSync {
		return
	}
	if b.cfg.Git.Origin == "" {
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
	auth := authFn(remote)
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
	go runReconcileLoop(ctx, wg, b.svc, hub, b.name, b.agentBranch, authFn, b.cfg.LocalOriginRoot)
}

// close releases resources opened so far. Safe to call at any point during
// the build sequence before build() — nil fields are skipped.
func (b *repoBuilder) close() {
	if b.svc != nil {
		b.svc.Close()
	}
}
