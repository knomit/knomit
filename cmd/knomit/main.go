package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/store"
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

			// 1. Open or init GitStore
			gitDBPath := filepath.Join(cfg.RepoPath, "knomit.git.db")
			gs, err := git.Open(gitDBPath)
			if err != nil {
				gs, err = git.Init(gitDBPath)
				if err != nil {
					return fmt.Errorf("open/init git store: %w", err)
				}
			}
			defer gs.Close()

			// 2. Open SearchIndex
			idxDBPath := filepath.Join(cfg.RepoPath, "knomit.index.db")
			idx, err := store.New(idxDBPath)
			if err != nil {
				return fmt.Errorf("open index: %w", err)
			}
			defer idx.Close()

			// 3. Initial sync
			if err := idx.Sync(gs); err != nil {
				log.Warn().Err(err).Msg("initial index sync failed")
			}

			// 4. Ensure embedder model files are present (downloads if missing), then load.
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

			// 5. Resolve LLM adapter
			ctx := context.Background()
			var llmAdapter llm.LLMAdapter
			provider, err := llm.ResolveProvider(cfg.LLM.Model, cfg.LLM.Provider)
			if err != nil {
				log.Warn().Err(err).Msg("LLM provider resolution failed")
			} else {
				llmAdapter, err = llm.NewAdapter(ctx, provider, cfg.LLM.Model)
				if err != nil {
					log.Warn().Err(err).Msg("LLM adapter init failed")
				}
			}

			// 6. Create per-profile MCP servers
			profiles := []string{"code", "chat", "generic"}
			mcpServers := make(map[string]http.Handler, len(profiles))
			for _, p := range profiles {
				mcpSrv := mcp.NewServer(gs, idx, llmAdapter, p, cfg.OntologyRoot)
				mcpServers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
			}

			// 7. Wire git remote if enabled
			var gitHandler http.Handler
			if cfg.Git.Remote {
				gitHandler = web.GitRemoteHandler(gs, cfg.LLM.APIKey)
			}

			// 8. Create TaskHub
			hub := web.NewTaskHub(ctx)

			// 9. Create synthesis dependencies
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
			router := web.NewRouter(gs, idx, hub, synthDeps, mcpServers, gitHandler, embeddingsEnabled)

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
			hub.Shutdown()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			gitDBPath := filepath.Join(cfg.RepoPath, "knomit.git.db")
			if err := os.MkdirAll(cfg.RepoPath, 0o755); err != nil {
				return err
			}
			gs, err := git.Init(gitDBPath)
			if err != nil {
				return fmt.Errorf("init: %w", err)
			}
			gs.Close()
			fmt.Printf("Initialized knomit repo at %s\n", cfg.RepoPath)
			return nil
		},
	}
}

func resetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Wipe all data (git store + search index) and start fresh",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			gitDB := filepath.Join(cfg.RepoPath, "knomit.git.db")
			idxDB := filepath.Join(cfg.RepoPath, "knomit.index.db")

			for _, f := range []string{gitDB, idxDB} {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove %s: %w", f, err)
				}
				// WAL/SHM sidecars
				os.Remove(f + "-wal")
				os.Remove(f + "-shm")
			}

			log.Info().Str("repo", cfg.RepoPath).Msg("all databases removed")
			return nil
		},
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
			gs, err := git.Open(filepath.Join(cfg.RepoPath, "knomit.git.db"))
			if err != nil {
				return fmt.Errorf("open git store: %w", err)
			}
			defer gs.Close()
			idx, err := store.New(filepath.Join(cfg.RepoPath, "knomit.index.db"))
			if err != nil {
				return fmt.Errorf("open index: %w", err)
			}
			defer idx.Close()
			if err := idx.Sync(gs); err != nil {
				return fmt.Errorf("rebuild: %w", err)
			}
			log.Info().Msg("Index rebuilt successfully")
			return nil
		},
	}
}
