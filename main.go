package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/mcp"
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
				Str("repo", cfg.RepoPath).
				Str("port", cfg.Port).
				Str("llm_provider", provider).
				Str("llm_model", cfg.LLM.Model).
				Msg("config loaded")

			// 1. Open unified store (single SQLite database)
			dbPath := filepath.Join(cfg.RepoPath, "knomit.db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()

			// 2. Open or init git on top of the shared storer
			var ontology *fact.Ontology
			gs, err := git.OpenWithStorer(svc.GitStorer())
			if err != nil {
				// First run: init with default ontology.
				ontology = fact.DefaultOntology()
				ontologyYAML, serErr := ontology.Serialize()
				if serErr != nil {
					return fmt.Errorf("serialize ontology: %w", serErr)
				}
				initFiles := map[string]string{
					"domains/ontology.yaml": string(ontologyYAML),
				}
				gs, err = git.InitWithStorer(svc.GitStorer(), initFiles)
				if err != nil {
					return fmt.Errorf("init git: %w", err)
				}
			} else {
				// Existing repo: load ontology from git.
				ontologyYAML, readErr := gs.ReadFile("domains/ontology.yaml")
				if readErr != nil {
					log.Warn().Msg("domains/ontology.yaml not found, using default ontology")
					ontology = fact.DefaultOntology()
				} else {
					ontology, err = fact.ParseOntology([]byte(ontologyYAML))
					if err != nil {
						return fmt.Errorf("parse ontology: %w", err)
					}
				}
			}

			idx := svc.Index()

			// 3. Ensure embedder model files are present (downloads if missing), then load.
			var embedder *embeddings.Embedder
			modelPath, tokPath, err := embeddings.EnsureModel(cfg.CacheDir)
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
				idx.SetEmbedder(embedder)
				defer embedder.Close()
			}

			// 4. Initial sync (must happen after embedder is attached so vectors are computed)
			if err := idx.Sync(gs, gs.Branch()); err != nil {
				log.Warn().Err(err).Msg("initial index sync failed")
			}

			// 4a. Create TaskHub (needed by observer below)
			ctx := context.Background()
			hub := web.NewTaskHub(ctx)

			// 4b. Observer: sync index + push SSE on every git commit.
			obs := newObserver(time.Second, func(hash string) {
				if err := idx.Sync(gs, gs.Branch()); err != nil {
					log.Warn().Err(err).Msg("observer sync failed")
				}
				hub.BroadcastStatus(hash)
			})
			defer obs.Stop()
			gs.SetOnCommit(obs.Notify)

			// 4c. Background remote sync + push goroutines.
			syncCtx, syncCancel := context.WithCancel(ctx)
			var syncWg sync.WaitGroup
			remote, _ := svc.GetRemote("origin")
			if remote != nil {
				if err := gs.ConfigureRemote(remote.URL, remote.Branch); err != nil {
					log.Warn().Err(err).Msg("remote: configure failed")
				} else {
					syncWg.Add(2)
					go runSyncLoop(syncCtx, &syncWg, gs, svc, hub, remote)
					go runPushLoop(syncCtx, &syncWg, gs, svc, hub, remote)
				}
			}

			// 5. Resolve LLM adapter
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

			// Optional LLM trace log (set KNOMIT_LLM_TRACE to a file path)
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

			// 6. Create per-profile MCP servers
			reviewer := &reviewerAdapter{r: synthesize.NewReviewer(gs, idx, idx, nil)}
			profiles := []string{"code", "chat", "generic"}
			mcpServers := make(map[string]http.Handler, len(profiles))
			for _, p := range profiles {
				mcpSrv := mcp.NewServer(gs, idx, idx, reviewer, p, cfg.OntologyRoot, ontology)
				mcpServers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
			}

			// 7. Wire git remote if enabled
			var gitHandler http.Handler
			if cfg.Git.Serve {
				gitHandler = web.GitRemoteHandler(gs, cfg.LLM.APIKey)
			}

			// 8. Create synthesis dependencies
			var synthDeps *web.SynthDeps
			if llmAdapter != nil {
				synthDeps = &web.SynthDeps{
					GS:       gs,
					Idx:      idx,
					Embedder: embedder,
					Adapter:  llmAdapter,
				}
				log.Info().Msg("synthesis enabled")
			} else {
				log.Warn().Msg("synthesis disabled (no LLM adapter)")
			}

			// 10. Create chi router
			router := web.NewRouter(gs, idx, hub, synthDeps, mcpServers, gitHandler, embeddingsEnabled, cfg.OntologyRoot)

			// 11. Graceful shutdown
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
				log.Info().Str("port", cfg.Port).Msg("knomit listening")
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal().Err(err).Msg("listen failed")
				}
			}()

			<-stop
			syncCancel()
			syncWg.Wait()
			hub.Shutdown()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
}

func initCmd() *cobra.Command {
	var ontologyPath string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := os.MkdirAll(cfg.RepoPath, 0o755); err != nil {
				return err
			}

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

			dbPath := filepath.Join(cfg.RepoPath, "knomit.db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()

			initFiles := map[string]string{
				"domains/ontology.yaml": string(ontologyYAML),
			}
			if _, err := git.InitWithStorer(svc.GitStorer(), initFiles); err != nil {
				return fmt.Errorf("init git: %w", err)
			}
			fmt.Printf("Initialized knomit repo at %s\n", cfg.RepoPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&ontologyPath, "ontology", "", "path to custom ontology YAML file")
	return cmd
}

func resetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Wipe all data and start fresh",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbFile := filepath.Join(cfg.RepoPath, "knomit.db")
			for _, f := range []string{dbFile} {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove %s: %w", f, err)
				}
				// WAL/SHM sidecars
				os.Remove(f + "-wal")
				os.Remove(f + "-shm")
			}

			log.Info().Str("repo", cfg.RepoPath).Msg("database removed")
			return nil
		},
	}
}

// reviewerAdapter adapts *synthesize.Reviewer to the mcp.Reviewer interface,
// widening the return type from *synthesize.ReviewResult to interface{}.
type reviewerAdapter struct {
	r *synthesize.Reviewer
}

func (a *reviewerAdapter) StartSession() (interface{}, error) {
	return a.r.StartSession()
}

func (a *reviewerAdapter) ContinueSession(sessionID, response string) (interface{}, error) {
	return a.r.ContinueSession(sessionID, response)
}

// runSyncLoop pulls from the configured remote on a fixed interval.
// First sync fires immediately, then every remote.Interval seconds.
func runSyncLoop(ctx context.Context, wg *sync.WaitGroup, gs *git.Store, svc *store.Service, hub *web.TaskHub, remote *store.Remote) {
	defer wg.Done()

	interval := time.Duration(remote.Interval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	doSync := func() {
		result, err := gs.Sync(remote.Branch)
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemoteStatus(remote.Name, "error", &errMsg)
			hub.BroadcastSyncError(remote.Name, errMsg)
			log.Warn().Err(err).Msg("remote sync failed")
			return
		}
		_ = svc.UpdateRemoteStatus(remote.Name, "ok", nil)
		if result.Synced {
			hub.BroadcastSyncOK(remote.Name, result.MergeCommit, result.FastForward)
			log.Info().
				Bool("fast_forward", result.FastForward).
				Str("merge_commit", result.MergeCommit).
				Msg("remote sync complete")
		}
	}

	// Immediate first sync.
	doSync()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSync()
		}
	}
}

// runPushLoop pushes the agent branch to origin on a fixed interval.
func runPushLoop(ctx context.Context, wg *sync.WaitGroup, gs *git.Store, svc *store.Service, hub *web.TaskHub, remote *store.Remote) {
	defer wg.Done()

	interval := time.Duration(remote.PushInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	doPush := func() {
		result, err := gs.Push()
		if err != nil {
			errMsg := err.Error()
			_ = svc.UpdateRemotePushStatus(remote.Name, "error", &errMsg)
			hub.BroadcastPushError(remote.Name, errMsg)
			log.Warn().Err(err).Msg("remote push failed")
			return
		}
		_ = svc.UpdateRemotePushStatus(remote.Name, "ok", nil)
		if result.Pushed {
			hub.BroadcastPushOK(remote.Name)
			log.Info().Str("branch", gs.Branch()).Msg("remote push complete")
		}
	}

	// Immediate first push.
	doPush()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doPush()
		}
	}
}

func rebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the search index from scratch",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbPath := filepath.Join(cfg.RepoPath, "knomit.db")
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
			log.Info().Msg("Index rebuilt successfully")
			return nil
		},
	}
}
