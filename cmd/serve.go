package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/synthesize"
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
			listenAddr := cfg.Host + ":" + cfg.Port
			httpAddr := "http://" + listenAddr

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
				Addr:              listenAddr,
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
		synthReviewer := synthesize.NewReviewer(result.gs, result.idx, result.idx, embedder, nil)
		result.ri.SynthDeps = &web.SynthDeps{
			GS:       result.gs,
			Idx:      result.idx,
			Embedder: embedder,
			Adapter:  llmAdapter,
			Reviewer: synthReviewer,
		}
	}
}
