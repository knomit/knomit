package repos

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

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

	if keyData, readErr := os.ReadFile(b.keyPath); readErr == nil {
		if crypt, cryptErr := store.NewCrypt(keyData); cryptErr == nil {
			svc.SetCrypt(crypt)
		}
	}
	return nil
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
	b.ontology = ont
}

// initDefaultGit creates the git store for the default ("knomit") repo on
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
		auth, authErr := resolveAuth(b.cfg.Remote, b.keyPath)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		// Pass empty upstreamMain — InitFromRemote will detect from the
		// remote's symbolic HEAD after the initial fetch, falling back to
		// "main" if detection fails. Capture the resolved value on the
		// builder so ensureBranch can pass it to SetRemote.
		b.upstreamMain = store.DetectRemoteUpstreamFromURL(b.cfg.Git.Origin, auth)
		if b.upstreamMain == "" {
			b.upstreamMain = "main"
		}
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
	if err := b.svc.IndexManager().Sync(context.Background(), b.agentBranch); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("initial index sync failed")
	}
	if b.cfg.Git.Origin != "" {
		upstream := b.upstreamMain
		if upstream == "" {
			upstream = "main"
		}
		if err := b.svc.IndexManager().Sync(context.Background(), upstream); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Str("branch", upstream).Msg("initial index sync (upstream) failed")
		}
	}
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
		svc:                 b.svc,
		hub:                 hub,
	}

	// Observer: sync index + push SSE on every git commit.
	obs := observe.New(time.Second, func(hash string) {
		ri.mu.RLock()
		currentSvc := ri.svc
		ri.mu.RUnlock()
		if err := currentSvc.IndexManager().Sync(context.Background(), b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Msg("observer sync failed")
		}
		hub.broadcastStatus(hash)
	})
	ri.onCommit = func(_, hash string) { obs.Notify(hash) }
	b.svc.SetOnCommit(ri.onCommit)

	// Startup recovery: if origin is configured and reachable, reconcile
	// once before the background loops start. This catches the
	// reinstall-with-state-intact and token-expired-then-fixed cases.
	b.recoverFromOrigin()

	// Background remote sync + push goroutines.
	syncCtx, syncCancel := context.WithCancel(b.ctx)
	var syncWg sync.WaitGroup
	b.startSyncLoops(syncCtx, &syncWg, hub)

	ri.syncCancel = syncCancel
	ri.syncWg = &syncWg

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
		go runReconcileLoop(newCtx, &syncWg, currentSvc, hub, name, agentBranch, authFn)
		return nil
	}

	ri.closeFn = func() {
		obs.Stop()
		ri.mu.RLock()
		svc := ri.svc
		ri.mu.RUnlock()
		svc.Close()
	}

	return ri
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
	remote, _ := b.svc.Remote().GetRemote("origin")
	if remote == nil {
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
	remote, _ := b.svc.Remote().GetRemote("origin")
	if remote == nil {
		return
	}

	authFn := makeRemoteAuthFn(b.cfg.Remote, b.keyPath)
	wg.Add(1)
	go runReconcileLoop(ctx, wg, b.svc, hub, b.name, b.agentBranch, authFn)
}

// close releases resources opened so far. Safe to call at any point during
// the build sequence before build() — nil fields are skipped.
func (b *repoBuilder) close() {
	if b.svc != nil {
		b.svc.Close()
	}
}
