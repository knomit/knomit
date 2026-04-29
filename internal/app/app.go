// Package app assembles the knomit application from its subsystems:
// config, embeddings, LLM, repo management, and the HTTP server.
package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/web"
)

// App holds the assembled application and its closeable resources.
type App struct {
	manager     *repos.Manager
	server      *web.Server
	signer      ssh.Signer
	agentBranch string

	closers []func()
}

func (a *App) Manager() *repos.Manager { return a.manager }
func (a *App) Server() *web.Server     { return a.server }
func (a *App) Signer() ssh.Signer      { return a.signer }
func (a *App) AgentBranch() string     { return a.agentBranch }

// New creates and boots the application from the given config and context.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	a := &App{}

	// SSH keypair.
	keyPath := cfg.Remote.SSHKey
	if keyPath == "" {
		keyPath = filepath.Join(cfg.Home, "id_ed25519")
	}
	signer, keyFingerprint, err := ensureKeyPair(keyPath)
	if err != nil {
		return nil, fmt.Errorf("ensure keypair: %w", err)
	}
	a.signer = signer
	a.agentBranch = agentBranch(keyFingerprint)

	// Embedder.
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
	if embedder != nil {
		a.closers = append(a.closers, embedder.Close)
	}

	// LLM adapter.
	var llmAdapter llm.LLMAdapter
	provider, err := llm.ResolveProvider(cfg.LLM.Model, cfg.LLM.Provider)
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
			a.closers = append(a.closers, func() { tracer.Close() })
			log.Info().Str("path", tracePath).Msg("LLM tracing enabled")
		}
	}

	if llmAdapter != nil {
		log.Info().Msg("synthesis enabled")
	} else {
		log.Warn().Msg("synthesis disabled (no LLM adapter)")
	}

	// Repo manager.
	a.manager = repos.New(ctx, repos.Deps{
		Cfg:         cfg,
		Signer:      signer,
		AgentBranch: a.agentBranch,
		Embedder:    embedder,
		KeyPath:     keyPath,
	})

	// Web server.
	var gitHandler http.Handler
	if cfg.Git.Serve {
		gitHandler = web.GitRemoteHandler(a.manager)
	}

	a.server = &web.Server{
		Manager:           a.manager,
		GitHandler:        gitHandler,
		EmbeddingsEnabled: embedder != nil,
		OntologyRoot:      cfg.OntologyRoot,
		AgentBranch:       a.agentBranch,
		SessionManager:    web.NewSessionManager(),
		LLMAdapter:        llmAdapter,
		Embedder:          embedder,
	}

	// Start the manager (opens repos, launches background cluster
	// checker). Manager owns its own internal lifecycle — app does not
	// reach into checker config or stop hooks.
	if err := a.manager.Start(); err != nil {
		a.Close()
		return nil, fmt.Errorf("start manager: %w", err)
	}

	return a, nil
}

// Handler returns the wired HTTP handler.
func (a *App) Handler() http.Handler {
	return a.server.Handler()
}

// Close shuts down repos and releases all resources.
func (a *App) Close() {
	_ = a.manager.Close()
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

