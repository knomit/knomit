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

	root := &cobra.Command{Use: "knomit", Short: "Git-backed knowledge base"}
	root.AddCommand(serveCmd())
	root.AddCommand(initCmd())
	root.AddCommand(rebuildCmd())
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
			cfg := config.FromEnv()

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

			// 4. Load embedder if model files present (optional)
			var embedder *embeddings.Embedder
			modelPath := filepath.Join(cfg.CacheDir, "model.onnx")
			tokPath := filepath.Join(cfg.CacheDir, "tokenizer.json")
			if _, statErr := os.Stat(modelPath); statErr == nil {
				embedder, err = embeddings.NewEmbedder(modelPath, tokPath)
				if err != nil {
					log.Warn().Err(err).Msg("embedder init failed")
				}
			}
			if embedder != nil {
				idx.SetEmbedder(embedder)
				defer embedder.Close()
			}

			// 5. Resolve LLM adapter
			ctx := context.Background()
			var llmAdapter llm.LLMAdapter
			provider, err := llm.ResolveProvider(cfg.LLMModel, cfg.LLMProvider)
			if err != nil {
				log.Warn().Err(err).Msg("LLM provider resolution failed")
			} else {
				llmAdapter, err = llm.NewAdapter(ctx, provider, cfg.LLMModel)
				if err != nil {
					log.Warn().Err(err).Msg("LLM adapter init failed")
				}
			}

			// 6. Create MCP server and HTTP handler
			mcpSrv := mcp.NewServer(gs, idx, llmAdapter, "code")
			mcpHandler := mcpserver.NewStreamableHTTPServer(mcpSrv)

			// 7. Wire git remote if enabled
			var gitHandler http.Handler
			if cfg.GitRemote {
				gitHandler = web.GitRemoteHandler(gs, cfg.APIKey)
			}

			// 8. Create chi router
			router := web.NewRouter(gs, idx, nil, mcpHandler, gitHandler)

			// 9. Graceful shutdown
			srv := &http.Server{
				Addr:    ":" + cfg.Port,
				Handler: router,
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
			cfg := config.FromEnv()
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

func rebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the search index from scratch",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.FromEnv()
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
