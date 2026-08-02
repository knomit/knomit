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

// Options holds CLI-only overrides that are not persisted to config.
type Options struct {
	// APIOnly omits the embedded web UI routes from the HTTP handler. The
	// desktop build sets this (Wails serves the UI in-process); the cloud
	// server leaves it false to serve UI + API together.
	APIOnly bool
	// CORSOrigins is the cross-origin allow-list passed to the web server (the
	// Wails origin in the desktop build). Empty in the cloud server.
	CORSOrigins []string
}

// New creates and boots the application from the given config and context.
//
// boot MUST come from Bootstrap, which has already restored KNOMIT_HOME from
// the replica. New opens databases; restore refuses to overwrite files that
// exist, so calling New first would turn every restore into a silent no-op.
// New takes the resolved identity rather than re-deriving it so the agent
// branch is computed exactly once — two independent derivations are two chances
// to disagree, and a disagreement is a silent fork.
func New(ctx context.Context, cfg config.Config, boot *BootResult, opts Options) (*App, error) {
	// Checked, not assumed: New is about to open databases, and restore refuses
	// to overwrite files that exist. A BootResult that did not come from
	// Bootstrap means no restore ran, so this would open an un-rehydrated
	// volume and then replicate its empty state over the good backup.
	if boot == nil || !boot.bootstrapped {
		return nil, fmt.Errorf("app.New: boot result did not come from app.Bootstrap — Bootstrap restores KNOMIT_HOME, and it must run before any database is opened")
	}
	a := &App{signer: boot.Signer, agentBranch: boot.AgentBranch}
	keyPath := keyPathFor(cfg)

	// Embedder. Embeddings are MANDATORY: every fact is indexed with a vector
	// and the per-model cosine thresholds are load-bearing for dedup, graph
	// density, and search recall. A service running without an embedder would
	// silently write vectorless facts and mis-tune retrieval, so failure to
	// build one is fatal rather than a degraded mode.
	model, err := embeddings.Lookup(cfg.Embeddings.Model)
	if err != nil {
		return nil, fmt.Errorf("embedder model config invalid (embeddings.model=%q): %w", cfg.Embeddings.Model, err)
	}
	embedder, err := embeddings.NewEmbedder(ctx, model, filepath.Join(cfg.Home, "models"))
	if err != nil {
		return nil, fmt.Errorf("embedder init failed for model %q (embeddings are required — check ONNX model files / network): %w", model.ID, err)
	}
	a.closers = append(a.closers, embedder.Close)
	log.Info().Str("model", model.ID).Int("dim", model.Dim).
		Msg("embedder enabled — facts indexed with vectors; semantic search and methodology vector ranking active")

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
	//
	// boot.Backup is a *backup.Manager, and a typed nil pointer stored in an
	// interface makes that interface NON-nil — repos would then see a live
	// tracker and take the backup-enabled path with replication switched off.
	// Assign through an explicit nil check so the interface is genuinely nil.
	var tracker repos.BackupTracker
	if boot.Backup != nil {
		tracker = boot.Backup
	}
	a.manager = repos.New(ctx, repos.Deps{
		Cfg:         cfg,
		Signer:      boot.Signer,
		AgentBranch: a.agentBranch,
		Embedder:    embedder,
		KeyPath:     keyPath,
		Backup:      tracker,
		// With replication running, a registered repo that silently fails to
		// open is not merely missing from the API: its now-empty local state
		// gets replicated OVER the good backup. Strictness is therefore tied to
		// backup being on, not to a separate switch.
	})

	// Web server.
	var gitHandler http.Handler
	if cfg.Git.Serve {
		gitHandler = web.GitRemoteHandler(a.manager)
	}

	a.server = &web.Server{
		Manager:           a.manager,
		GitHandler:        gitHandler,
		EmbeddingsEnabled: true, // mandatory: New returns an error above if absent.
		OntologyRoot:      cfg.OntologyRoot,
		AgentBranch:       a.agentBranch,
		SessionManager:    web.NewSessionManager(),
		LLMAdapter:        llmAdapter,
		Embedder:          embedder,
		APIOnly:           opts.APIOnly,
		CORSOrigins:       opts.CORSOrigins,
		ReadOnly:          cfg.ReadOnly,
		SlowRequestMS:     cfg.Log.SlowRequestMS,
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
	if err := a.manager.Close(); err != nil {
		log.Warn().Err(err).Msg("app: manager close failed")
	}
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}
