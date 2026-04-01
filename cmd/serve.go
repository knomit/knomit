package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/web"
)

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
			signer, keyFingerprint, err := EnsureKeyPair(keyPath)
			if err != nil {
				return fmt.Errorf("ensure keypair: %w", err)
			}
			agentBranch := AgentBranch(keyFingerprint)

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
			ctx := cmd.Context()
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

			// 3. Boot all repositories.
			m := repos.New(ctx, repos.Deps{
				Cfg:         cfg,
				Signer:      signer,
				AgentBranch: agentBranch,
				Embedder:    embedder,
				LLM:         llmAdapter,
				KeyPath:     keyPath,
			})
			if err := m.Boot(); err != nil {
				return fmt.Errorf("boot: %w", err)
			}

			// 4. Wire git remote handler (all repos via Manager).
			var gitHandler http.Handler
			if cfg.Git.Serve {
				gitHandler = web.GitRemoteHandler(m)
			}

			// 5. Create chi router.
			router := web.NewRouter(m, gitHandler, embeddingsEnabled, cfg.OntologyRoot, agentBranch)

			// 6. Startup summary.
			pubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			listenAddr := cfg.Host + ":" + cfg.Port
			httpAddr := "http://" + listenAddr

			startupLog := log.Info().
				Str("http", httpAddr).
				Str("api", httpAddr+"/api/v1/{repo}").
				Str("mcp", httpAddr+"/api/v1/{repo}/mcp")

			if gitHandler != nil {
				startupLog = startupLog.Str("git_remote", httpAddr+"/git")
			}

			startupLog.
				Str("public_key", pubKey).
				Str("branch", agentBranch).
				Strs("repos", m.Names()).
				Msg("knomit ready")

			// 7. Graceful shutdown.
			srv := &http.Server{
				Addr:              listenAddr,
				Handler:           router,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      0, // 0 = no limit for SSE long-poll
				IdleTimeout:       60 * time.Second,
			}

			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal().Err(err).Msg("listen failed")
				}
			}()

			// pprof debug server (localhost only).
			if debugAddr := os.Getenv("KNOMIT_PPROF_ADDR"); debugAddr != "" {
				go func() {
					log.Info().Str("pprof", "http://"+debugAddr+"/debug/pprof/").Msg("pprof listening")
					if err := http.ListenAndServe(debugAddr, nil); err != nil {
						log.Warn().Err(err).Msg("pprof server failed")
					}
				}()
			}

			// Optional Unix socket listener.
			if cfg.Socket != "" {
				_ = os.Remove(cfg.Socket) // clean up stale socket
				ul, err := net.Listen("unix", cfg.Socket)
				if err != nil {
					log.Fatal().Err(err).Str("socket", cfg.Socket).Msg("unix socket listen failed")
				}
				defer ul.Close()
				defer os.Remove(cfg.Socket)
				log.Info().Str("socket", cfg.Socket).Msg("unix socket listening")
				go func() {
					if err := srv.Serve(ul); err != nil && err != http.ErrServerClosed {
						log.Fatal().Err(err).Msg("unix socket serve failed")
					}
				}()
			}

			<-ctx.Done()
			m.Shutdown()
			// Shared resource cleanup (embedder, tracer) happens via defers.
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
}
