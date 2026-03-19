package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/observe"
	"knomit/internal/store"
	"knomit/internal/synthesize"
	"knomit/internal/web"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	_ = godotenv.Load() // .env is optional

	root := &cobra.Command{Use: "knomit", Short: "Git-backed knowledge base"}
	root.AddCommand(serveCmd())
	root.AddCommand(initCmd())
	root.AddCommand(rebuildCmd())
	root.AddCommand(resetCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

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

	// Per-repo MCP servers.
	var mcpHandlers map[string]http.Handler
	if ontology != nil {
		reviewer := synthesize.NewReviewer(gs, idx, idx, nil, nil)
		profiles := []string{"code", "chat", "generic"}
		mcpHandlers = make(map[string]http.Handler, len(profiles))
		for _, p := range profiles {
			mcpSrv := mcp.NewServer(gs, idx, idx, reviewer, p, ontologyRoot, ontology)
			mcpHandlers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
		}
	}

	// Per-repo synthesis deps.
	var synthDeps *web.SynthDeps
	if llmAdapter != nil {
		synthDeps = &web.SynthDeps{
			GS:       gs,
			Idx:      idx,
			Embedder: embedder,
			Adapter:  llmAdapter,
		}
	}

	ri := &web.RepoInstance{
		Name:        name,
		GS:          gs,
		Svc:         svc,
		Idx:         idx,
		Hub:         hub,
		SyncCancel:  syncCancel,
		SyncWg:      &syncWg,
		MCPHandlers: mcpHandlers,
		SynthDeps:   synthDeps,
	}
	ri.StartSync = func(remoteURL string) error {
		remote, err := svc.GetRemote("origin")
		if err != nil || remote == nil {
			return fmt.Errorf("read remote: %w", err)
		}

		// Build auth from stored remote credentials.
		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := git.ResolveAuthWithOrigin(authCfg, keyPath, remoteURL)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		gs.SetAuth(auth)

		if err := gs.ConfigureRemote(remoteURL, remote.Branch); err != nil {
			return fmt.Errorf("configure remote: %w", err)
		}

		// Stop existing sync/push loops (if any) before starting new ones.
		syncCancel()
		syncWg.Wait()

		// Create fresh context and update ri so shutdown cancels the right one.
		syncCtx, syncCancel = context.WithCancel(ctx)
		ri.SyncCancel = syncCancel

		syncWg.Add(2)
		go runSyncLoop(syncCtx, &syncWg, gs, svc, hub, remote, name)
		go runPushLoop(syncCtx, &syncWg, gs, svc, hub, remote, name)
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

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the knomit HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			provider, _ := llm.ResolveProvider(cfg.LLM.Model, cfg.LLM.Provider)
			log.Info().
				Str("repo", cfg.Home).
				Str("port", cfg.Port).
				Str("llm_provider", provider).
				Str("llm_model", cfg.LLM.Model).
				Msg("config loaded")

			// 0. Ensure SSH keypair exists.
			keyPath := cfg.Remote.SSHKey
			if keyPath == "" {
				keyPath = filepath.Join(cfg.Home, "id_ed25519")
			}
			signer, keyFingerprint, err := git.EnsureKeyPair(keyPath)
			if err != nil {
				return fmt.Errorf("ensure keypair: %w", err)
			}
			agentBranch := git.AgentBranch(keyFingerprint)

			// 1. Ensure embedder model files are present (shared across repos).
			var embedder *embeddings.Embedder
			modelPath, tokPath, err := embeddings.EnsureModel(filepath.Join(cfg.Home, "models"))
			if err != nil {
				log.Warn().Err(err).Msg("embedder model unavailable")
			} else {
				embedder, err = embeddings.NewEmbedder(modelPath, tokPath)
				if err != nil {
					log.Warn().Err(err).Msg("embedder init failed")
				}
			}
			embeddingsEnabled := embedder != nil
			if embedder != nil {
				defer embedder.Close()
			}

			// 2. Resolve LLM adapter (shared across repos).
			ctx := context.Background()
			var llmAdapter llm.LLMAdapter
			provider, err = llm.ResolveProvider(cfg.LLM.Model, cfg.LLM.Provider)
			if err != nil {
				log.Warn().Err(err).Msg("LLM provider resolution failed")
			} else {
				llmAdapter, err = llm.NewAdapter(ctx, provider, cfg.LLM.Model, cfg.LLM)
				if err != nil {
					log.Warn().Err(err).Msg("LLM adapter init failed")
				}
			}

			if tracePath := os.Getenv("KNOMIT_LLM_TRACE"); tracePath != "" && llmAdapter != nil {
				tracer, err := llm.NewTracingAdapter(llmAdapter, tracePath)
				if err != nil {
					log.Warn().Err(err).Msg("LLM trace init failed")
				} else {
					llmAdapter = tracer
					defer tracer.Close()
					log.Info().Str("path", tracePath).Msg("LLM tracing enabled")
				}
			}

			if llmAdapter != nil {
				log.Info().Msg("synthesis enabled")
			} else {
				log.Warn().Msg("synthesis disabled (no LLM adapter)")
			}

			// 3. Discover repos — scan repos/*.db
			reposDir := filepath.Join(cfg.Home, "repos")
			if err := os.MkdirAll(reposDir, 0o755); err != nil {
				return fmt.Errorf("create repos dir: %w", err)
			}

			// Phase 1: Open default knomit repo first (needed for ontology).
			defaultDB := filepath.Join(reposDir, "knomit.db")
			knomitResult, err := openRepo(ctx, "knomit", defaultDB, true, signer, agentBranch, embedder, llmAdapter, cfg.OntologyRoot, nil, cfg, keyPath)
			if err != nil {
				return fmt.Errorf("open default repo: %w", err)
			}

			// Load ontology from knomit repo's git store.
			var ontology *fact.Ontology
			ontologyYAML, readErr := knomitResult.gs.ReadFile("domains/ontology.yaml")
			if readErr != nil {
				log.Warn().Msg("domains/ontology.yaml not found, using default ontology")
				ontology = fact.DefaultOntology()
			} else {
				ontology, err = fact.ParseOntology([]byte(ontologyYAML))
				if err != nil {
					knomitResult.cleanup()
					return fmt.Errorf("parse ontology: %w", err)
				}
			}

			// Now set MCP servers on the knomit repo (they need ontology).
			setRepoMCP(knomitResult, cfg.OntologyRoot, ontology, llmAdapter, embedder)

			rm := web.NewRepoManager()
			var allResults []*repoResult
			rm.Set("knomit", knomitResult.ri)
			allResults = append(allResults, knomitResult)

			// Phase 2: Discover and open remaining repos.
			dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
			sort.Strings(dbFiles)

			for _, dbPath := range dbFiles {
				base := filepath.Base(dbPath)
				name := strings.TrimSuffix(base, ".db")

				if name == "knomit" {
					continue // already opened
				}

				if !isValidRepoName(name) {
					log.Warn().Str("file", base).Msg("skipping db with invalid repo name")
					continue
				}

				result, err := openRepo(ctx, name, dbPath, false, signer, agentBranch, embedder, llmAdapter, cfg.OntologyRoot, ontology, cfg, keyPath)
				if err != nil {
					log.Warn().Err(err).Str("repo", name).Msg("skipping repo")
					continue
				}

				rm.Set(name, result.ri)
				allResults = append(allResults, result)
			}

			// 4. Wire git remote handler (all repos via RepoManager).
			var gitHandler http.Handler
			if cfg.Git.Serve {
				gitHandler = web.GitRemoteHandler(rm)
			}

			// 5. Create chi router.
			router := web.NewRouter(rm, gitHandler, embeddingsEnabled, cfg.OntologyRoot)

			// 6. Startup summary.
			pubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			httpAddr := "http://localhost:" + cfg.Port

			startupLog := log.Info().
				Str("http", httpAddr).
				Str("api", httpAddr+"/api/v1/{repo}").
				Str("mcp", httpAddr+"/api/v1/{repo}/mcp")

			if gitHandler != nil {
				startupLog = startupLog.Str("git_remote", httpAddr+"/git")
			}

			var repoNames []string
			rm.ForEach(func(name string, _ *web.RepoInstance) {
				repoNames = append(repoNames, name)
			})

			startupLog.
				Str("public_key", pubKey).
				Str("branch", agentBranch).
				Strs("repos", repoNames).
				Msg("knomit ready")

			// 7. Graceful shutdown.
			srv := &http.Server{
				Addr:              ":" + cfg.Port,
				Handler:           router,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      0, // 0 = no limit for SSE long-poll
				IdleTimeout:       60 * time.Second,
			}

			stop := make(chan os.Signal, 1)
			signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal().Err(err).Msg("listen failed")
				}
			}()

			<-stop
			// Cancel all sync loops first.
			for _, result := range allResults {
				result.ri.SyncCancel()
			}
			// Wait for all sync loops and clean up per-repo resources.
			for _, result := range allResults {
				result.ri.SyncWg.Wait()
				result.ri.Hub.Shutdown()
				result.cleanup()
			}
			// Shared resource cleanup (embedder, tracer) happens via defers.
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
}

// setRepoMCP creates and attaches MCP handlers to a repoResult.
// Called after the ontology is loaded (since MCP servers need it).
func setRepoMCP(result *repoResult, ontologyRoot string, ontology *fact.Ontology, llmAdapter llm.LLMAdapter, embedder *embeddings.Embedder) {
	reviewer := synthesize.NewReviewer(result.gs, result.idx, result.idx, embedder, nil)
	profiles := []string{"code", "chat", "generic"}
	mcpHandlers := make(map[string]http.Handler, len(profiles))
	for _, p := range profiles {
		mcpSrv := mcp.NewServer(result.gs, result.idx, result.idx, reviewer, p, ontologyRoot, ontology)
		mcpHandlers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
	}
	result.ri.MCPHandlers = mcpHandlers

	if llmAdapter != nil {
		result.ri.SynthDeps = &web.SynthDeps{
			GS:       result.gs,
			Idx:      result.idx,
			Embedder: embedder,
			Adapter:  llmAdapter,
		}
	}
}

func initCmd() *cobra.Command {
	var ontologyPath string
	var repoName string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			reposDir := filepath.Join(cfg.Home, "repos")
			if err := os.MkdirAll(reposDir, 0o755); err != nil {
				return err
			}

			// Ensure SSH keypair exists.
			keyPath := cfg.Remote.SSHKey
			if keyPath == "" {
				keyPath = filepath.Join(cfg.Home, "id_ed25519")
			}
			_, keyFingerprint, err := git.EnsureKeyPair(keyPath)
			if err != nil {
				return fmt.Errorf("ensure keypair: %w", err)
			}
			agentBranch := git.AgentBranch(keyFingerprint)

			// Load ontology: custom file or embedded default.
			ontology := fact.DefaultOntology()
			if ontologyPath != "" {
				data, err := os.ReadFile(ontologyPath)
				if err != nil {
					return fmt.Errorf("read ontology file: %w", err)
				}
				ontology, err = fact.ParseOntology(data)
				if err != nil {
					return fmt.Errorf("parse ontology: %w", err)
				}
			}
			ontologyYAML, err := ontology.Serialize()
			if err != nil {
				return fmt.Errorf("serialize ontology: %w", err)
			}

			dbPath := filepath.Join(reposDir, repoName+".db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()

			initFiles := map[string]string{
				"domains/ontology.yaml": string(ontologyYAML),
			}
			if _, err := git.InitWithStorer(svc.GitStorer(), initFiles, agentBranch); err != nil {
				return fmt.Errorf("init git: %w", err)
			}
			fmt.Printf("Initialized knomit repo %q at %s\n", repoName, dbPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&ontologyPath, "ontology", "", "path to custom ontology YAML file")
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}

func resetCmd() *cobra.Command {
	var repoName string
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe all data and start fresh",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbFile := filepath.Join(cfg.Home, "repos", repoName+".db")
			for _, f := range []string{dbFile} {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove %s: %w", f, err)
				}
				// WAL/SHM sidecars
				os.Remove(f + "-wal")
				os.Remove(f + "-shm")
			}

			log.Info().Str("repo", repoName).Msg("database removed")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name to reset")
	return cmd
}


// runSyncLoop pulls from the configured remote on a fixed interval.
// First sync fires immediately, then every remote.Interval seconds.
// The interval is re-read from the database on each tick so that changes
// made via PUT /api/v1/{repo}/origin take effect without a restart.
func runSyncLoop(ctx context.Context, wg *sync.WaitGroup, gs *git.Store, svc *store.Service, hub *web.TaskHub, remote *store.Remote, repo string) {
	defer wg.Done()

	interval := time.Duration(remote.Interval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("sync loop started")

	doSync := func() {
		result, err := gs.Sync(remote.Branch)
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemoteStatus(remote.Name, "error", &errMsg)
			hub.BroadcastSyncError(remote.Name, errMsg)
			lg.Warn().Err(err).Msg("sync: pull failed")
			return
		}
		_ = svc.UpdateRemoteStatus(remote.Name, "ok", nil)
		if result.Synced {
			hub.BroadcastSyncOK(remote.Name, result.MergeCommit, result.FastForward)
			lg.Info().
				Bool("fast_forward", result.FastForward).
				Str("merge_commit", result.MergeCommit).
				Msg("sync: pulled changes")
		} else {
			lg.Debug().Msg("sync: up to date")
		}
	}

	// Immediate first sync.
	doSync()

	for {
		select {
		case <-ctx.Done():
			lg.Info().Msg("sync loop stopped")
			return
		case <-ticker.C:
			// Re-read remote config so interval changes via PUT /origin take effect.
			if fresh, err := svc.GetRemote(remote.Name); err == nil && fresh != nil {
				if d := time.Duration(fresh.Interval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("sync: interval changed")
					interval = d
					ticker.Reset(interval)
				}
			}
			doSync()
		}
	}
}

// runPushLoop pushes the agent branch to origin on a fixed interval.
// The interval is re-read from the database on each tick so that changes
// made via PUT /api/v1/{repo}/origin take effect without a restart.
func runPushLoop(ctx context.Context, wg *sync.WaitGroup, gs *git.Store, svc *store.Service, hub *web.TaskHub, remote *store.Remote, repo string) {
	defer wg.Done()

	interval := time.Duration(remote.PushInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Dur("interval", interval).Msg("push loop started")

	doPush := func() {
		result, err := gs.Push()
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemotePushStatus(remote.Name, "error", &errMsg)
			hub.BroadcastPushError(remote.Name, errMsg)
			lg.Warn().Err(err).Msg("push: failed")
			return
		}
		_ = svc.UpdateRemotePushStatus(remote.Name, "ok", nil)
		if result.Pushed {
			hub.BroadcastPushOK(remote.Name)
			lg.Info().Str("branch", gs.Branch()).Msg("push: pushed changes")
		} else {
			lg.Debug().Msg("push: up to date")
		}
	}

	// Immediate first push.
	doPush()

	for {
		select {
		case <-ctx.Done():
			lg.Info().Msg("push loop stopped")
			return
		case <-ticker.C:
			// Re-read remote config so interval changes via PUT /origin take effect.
			if fresh, err := svc.GetRemote(remote.Name); err == nil && fresh != nil {
				if d := time.Duration(fresh.PushInterval) * time.Second; d != interval {
					lg.Info().Dur("old", interval).Dur("new", d).Msg("push: interval changed")
					interval = d
					ticker.Reset(interval)
				}
			}
			doPush()
		}
	}
}

func rebuildCmd() *cobra.Command {
	var repoName string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the search index from scratch",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbPath := filepath.Join(cfg.Home, "repos", repoName+".db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()
			gs, err := git.OpenWithStorer(svc.GitStorer())
			if err != nil {
				return fmt.Errorf("open git: %w", err)
			}
			idx := svc.Index()
			if err := idx.Sync(gs, gs.Branch()); err != nil {
				return fmt.Errorf("rebuild: %w", err)
			}
			log.Info().Str("repo", repoName).Msg("Index rebuilt successfully")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}
