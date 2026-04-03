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
	name        string
	dbPath      string
	isDefault   bool
	cfg         config.Config
	signer      ssh.Signer
	agentBranch string
	embedder    store.BatchEmbedder
	keyPath     string
	ctx         context.Context

	// accumulated state
	svc      *store.Service
	idx      store.SearchIndex
	ontology *fact.Ontology
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
	if b.cfg.Git.Origin != "" {
		auth, authErr := resolveAuth(b.cfg.Remote, b.keyPath)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		if err := b.svc.InitFromRemote(b.cfg.Git.Origin, auth, b.agentBranch); err != nil {
			return fmt.Errorf("init from remote: %w", err)
		}
		return nil
	}

	ont := fact.DefaultOntology()
	ontologyYAML, err := ont.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}
	if err := b.svc.InitRepo(map[string]string{
		"domains/ontology.yaml": string(ontologyYAML),
	}, b.agentBranch); err != nil {
		return fmt.Errorf("init git: %w", err)
	}
	return nil
}

// ensureBranch creates the agent branch if it doesn't already exist and seeds
// the origin remote record for the default repo.
func (b *repoBuilder) ensureBranch() {
	if b.agentBranch != "" {
		if err := b.svc.CreateBranch(context.Background(), b.agentBranch, b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Msg("branch create/ensure failed")
		}
	}
	if b.isDefault && b.cfg.Git.Origin != "" {
		if err := b.svc.SetRemote("origin", b.cfg.Git.Origin, "main", 300, 300); err != nil {
			log.Warn().Err(err).Msg("failed to seed origin in remotes table")
		}
	}
}

// setupIndex configures the search index with the embedder and runs an initial
// sync against the git store.
func (b *repoBuilder) setupIndex() {
	b.idx = b.svc.Search()
	if b.embedder != nil {
		b.idx.SetEmbedder(b.embedder)
	}
	if err := b.idx.Sync(context.Background(), b.svc, b.agentBranch); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("initial index sync failed")
	}
}

// seedWatermarks sets the pipeline watermark to HEAD for any tool that has no
// watermark for the current agent branch, so the first pipeline run only
// processes facts written after this point.
func (b *repoBuilder) seedWatermarks() {
	for _, tool := range []string{"review", "hypothesize"} {
		if wm, _ := b.svc.Pipeline().GetPipelineWatermark(context.Background(), tool, b.agentBranch); wm == "" {
			if head, err := b.svc.HeadCommit(context.Background(), b.agentBranch); err == nil {
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
// seedWatermarks. The returned instance is ready for SetupMCP and registration.
func (b *repoBuilder) build() *RepoInstance {
	hub := NewTaskHub(b.ctx)

	// Allocate ri first — the observer and closures capture the pointer so
	// they follow SwapStore field replacements via the read lock.
	ri := &RepoInstance{
		name:        b.name,
		dbPath:      b.dbPath,
		agentBranch: b.agentBranch,
		ontology:    b.ontology,
		svc:         b.svc,
		idx:         b.idx,
		hub:         hub,
	}

	// Observer: sync index + push SSE on every git commit.
	obs := observe.New(time.Second, func(hash string) {
		ri.mu.RLock()
		currentSvc := ri.svc
		ri.mu.RUnlock()
		if err := currentSvc.Search().Sync(context.Background(), currentSvc, b.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", b.name).Msg("observer sync failed")
		}
		hub.broadcastStatus(hash)
	})
	ri.onCommit = func(_, hash string) { obs.Notify(hash) }
	b.svc.SetOnCommit(ri.onCommit)

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

		remote, err := currentSvc.GetRemote("origin")
		if err != nil || remote == nil {
			return fmt.Errorf("read remote: %w", err)
		}

		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := ResolveAuthWithOrigin(authCfg, keyPath, remoteURL)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		currentSvc.SetAuth(auth)

		if err := currentSvc.ConfigureRemote(context.Background(), remoteURL, remote.Branch); err != nil {
			return fmt.Errorf("configure remote: %w", err)
		}

		syncCancel()
		syncWg.Wait()

		var newCtx context.Context
		newCtx, syncCancel = context.WithCancel(ctx)
		ri.syncCancel = syncCancel

		currentSvc.SetOnCommit(ri.onCommit)

		syncWg.Add(2)
		go runSyncLoop(newCtx, &syncWg, currentSvc, hub, remote, name, agentBranch)
		go runPushLoop(newCtx, &syncWg, currentSvc, hub, remote, name, agentBranch)
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

// startSyncLoops launches the background pull and push goroutines if a remote
// named "origin" is configured.
func (b *repoBuilder) startSyncLoops(ctx context.Context, wg *sync.WaitGroup, hub *TaskHub) {
	remote, _ := b.svc.GetRemote("origin")
	if remote == nil {
		return
	}

	authCfg := remoteAuthFromRecord(remote, b.cfg.Remote)
	auth, authErr := ResolveAuthWithOrigin(authCfg, b.keyPath, remote.URL)
	if authErr != nil {
		log.Warn().Err(authErr).Str("repo", b.name).Msg("remote: auth resolution failed")
		return
	}
	b.svc.SetAuth(auth)

	if err := b.svc.ConfigureRemote(context.Background(), remote.URL, remote.Branch); err != nil {
		log.Warn().Err(err).Str("repo", b.name).Msg("remote: configure failed")
		return
	}

	wg.Add(2)
	go runSyncLoop(ctx, wg, b.svc, hub, remote, b.name, b.agentBranch)
	go runPushLoop(ctx, wg, b.svc, hub, remote, b.name, b.agentBranch)
}

// close releases resources opened so far. Safe to call at any point during
// the build sequence before build() — nil fields are skipped.
func (b *repoBuilder) close() {
	if b.svc != nil {
		b.svc.Close()
	}
}
