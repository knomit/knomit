package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/observe"
	"knomit/internal/store"
	"knomit/internal/web"
)

// repoResult holds the initialized resources for a single repo.
type repoResult struct {
	ri      *web.RepoInstance
	gs      *git.Store
	svc     *store.Service
	idx     *store.Index // concrete index — needed for MCP/synthesize which require wider interfaces
	obs     *observe.Observer
	cleanup func() // close svc, stop observer — NOT the embedder or LLM adapter
}

// openRepo initialises a single repo from a SQLite database file.
// Shared resources (signer, embedder, LLM adapter) are passed in but
// never closed by this function — their lifecycle is managed by the caller.
//
// If isDefault is true and no git data exists, the repo is initialised
// from scratch (or cloned from origin). Non-default repos that fail to
// open are returned as errors so the caller can skip them gracefully.
func openRepo(
	ctx context.Context,
	name string,
	dbPath string,
	isDefault bool,
	signer ssh.Signer,
	agentBranch string,
	embedder *embeddings.Embedder,
	llmAdapter llm.LLMAdapter,
	ontologyRoot string,
	ontology *fact.Ontology,
	cfg config.Config,
	keyPath string,
) (*repoResult, error) {
	svc, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Set up credential encryption using the SSH private key.
	if keyData, readErr := os.ReadFile(keyPath); readErr == nil {
		if crypt, cryptErr := store.NewCrypt(keyData); cryptErr == nil {
			svc.SetCrypt(crypt)
		}
	}

	freshInit := false
	gs, err := git.OpenWithStorer(svc.GitStorer())
	if err != nil {
		if !isDefault {
			svc.Close()
			return nil, fmt.Errorf("open git: %w", err)
		}
		freshInit = true
		// Default repo — first run, init from remote or local.
		if cfg.Git.Origin != "" {
			auth, authErr := git.ResolveAuth(cfg.Remote, keyPath)
			if authErr != nil {
				svc.Close()
				return nil, fmt.Errorf("resolve auth: %w", authErr)
			}
			gs, err = git.InitFromRemote(svc.GitStorer(), cfg.Git.Origin, auth, agentBranch)
			if err != nil {
				svc.Close()
				return nil, fmt.Errorf("init from remote: %w", err)
			}
		} else {
			ont := fact.DefaultOntology()
			ontologyYAML, serErr := ont.Serialize()
			if serErr != nil {
				svc.Close()
				return nil, fmt.Errorf("serialize ontology: %w", serErr)
			}
			initFiles := map[string]string{
				"domains/ontology.yaml": string(ontologyYAML),
			}
			gs, err = git.InitWithStorer(svc.GitStorer(), initFiles, agentBranch)
			if err != nil {
				svc.Close()
				return nil, fmt.Errorf("init git: %w", err)
			}
		}
	}

	gs.SetSigner(signer)

	// Switch to the expected agent branch if the repo is on a different one.
	if agentBranch != "" && gs.Branch() != agentBranch {
		if err := gs.SwitchBranch(agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("branch switch failed")
		}
	}

	// Seed remotes table for default repo on first startup.
	if isDefault && cfg.Git.Origin != "" {
		if err := svc.SetRemote("origin", cfg.Git.Origin, "main", 300, 300); err != nil {
			log.Warn().Err(err).Msg("failed to seed origin in remotes table")
		}
	}

	idx := svc.Index()
	if embedder != nil {
		idx.SetEmbedder(embedder)
	}

	// Initial index sync.
	if err := idx.Sync(gs, gs.Branch()); err != nil {
		log.Warn().Err(err).Str("repo", name).Msg("initial index sync failed")
	}

	// On fresh init, set the review watermark to HEAD so the first review
	// doesn't treat every existing fact as dirty.
	if freshInit {
		if head, err := gs.HeadCommit(); err == nil {
			if err := idx.SetReviewWatermark(gs.Branch(), head); err != nil {
				log.Warn().Err(err).Msg("review watermark: initial set failed")
			}
		}
	}

	hub := web.NewTaskHub(ctx)

	// Observer: sync index + push SSE on every git commit.
	obs := observe.New(time.Second, func(hash string) {
		if err := idx.Sync(gs, gs.Branch()); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("observer sync failed")
		}
		hub.BroadcastStatus(hash)
	})
	gs.SetOnCommit(obs.Notify)

	// Background remote sync + push goroutines.
	syncCtx, syncCancel := context.WithCancel(ctx)
	var syncWg sync.WaitGroup
	remote, _ := svc.GetRemote("origin")
	if remote != nil {
		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := git.ResolveAuthWithOrigin(authCfg, keyPath, remote.URL)
		if authErr != nil {
			log.Warn().Err(authErr).Str("repo", name).Msg("remote: auth resolution failed")
		} else {
			gs.SetAuth(auth)
		}

		if err := gs.ConfigureRemote(remote.URL, remote.Branch); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("remote: configure failed")
		} else {
			syncWg.Add(2)
			go runSyncLoop(syncCtx, &syncWg, gs, svc, hub, remote, name)
			go runPushLoop(syncCtx, &syncWg, gs, svc, hub, remote, name)
		}
	}

	ri := &web.RepoInstance{
		Name:       name,
		DBPath:     dbPath,
		GS:         gs,
		Svc:        svc,
		Idx:        idx,
		Hub:        hub,
		SyncCancel: syncCancel,
		SyncWg:     &syncWg,
	}
	ri.StartSync = func(remoteURL string) error {
		// Use ri.GS and ri.Svc (not captured gs/svc) so that after SwapStore
		// the sync loops operate on the current store, not the original one.
		currentGS, ok := ri.GS.(*git.Store)
		if !ok {
			return fmt.Errorf("current store is not a *git.Store")
		}
		currentSvc := ri.Svc

		remote, err := currentSvc.GetRemote("origin")
		if err != nil || remote == nil {
			return fmt.Errorf("read remote: %w", err)
		}

		// Build auth from stored remote credentials.
		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := git.ResolveAuthWithOrigin(authCfg, keyPath, remoteURL)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		currentGS.SetAuth(auth)

		if err := currentGS.ConfigureRemote(remoteURL, remote.Branch); err != nil {
			return fmt.Errorf("configure remote: %w", err)
		}

		// Stop existing sync/push loops (if any) before starting new ones.
		syncCancel()
		syncWg.Wait()

		// Create fresh context and update ri so shutdown cancels the right one.
		syncCtx, syncCancel = context.WithCancel(ctx)
		ri.SyncCancel = syncCancel

		syncWg.Add(2)
		go runSyncLoop(syncCtx, &syncWg, currentGS, currentSvc, hub, remote, name)
		go runPushLoop(syncCtx, &syncWg, currentGS, currentSvc, hub, remote, name)
		return nil
	}

	return &repoResult{
		ri:  ri,
		gs:  gs,
		svc: svc,
		idx: idx,
		obs: obs,
		cleanup: func() {
			obs.Stop()
			svc.Close()
		},
	}, nil
}

// remoteAuthFromRecord builds a RemoteAuthConfig from a stored remote record,
// falling back to the global config for fields not set in the record.
func remoteAuthFromRecord(remote *store.Remote, fallback git.RemoteAuthConfig) git.RemoteAuthConfig {
	cfg := fallback
	if remote.AuthMethod != "" {
		cfg.AuthMethod = remote.AuthMethod
	}
	if remote.AuthToken != "" {
		if cfg.AuthMethod == "basic" {
			// token field stores user:password
			if parts := strings.SplitN(remote.AuthToken, ":", 2); len(parts) == 2 {
				cfg.User = parts[0]
				cfg.Password = parts[1]
			}
		} else {
			cfg.Token = remote.AuthToken
		}
	}
	return cfg
}

// isValidRepoName checks that a repo name contains only lowercase letters,
// digits, hyphens, or underscores.
func isValidRepoName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
