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
	"knomit/internal/memlimit"
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
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
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

	// Embedder. Embeddings are MANDATORY: every fact is indexed with a vector
	// and the per-model cosine thresholds are load-bearing for dedup, graph
	// density, and search recall. A service running without an embedder would
	// silently write vectorless facts and mis-tune retrieval, so failure to
	// build one is fatal rather than a degraded mode.
	model, err := embeddings.Lookup(cfg.Embeddings.Model)
	if err != nil {
		return nil, fmt.Errorf("embedder model config invalid (embeddings.model=%q): %w", cfg.Embeddings.Model, err)
	}
	// 0 is the documented auto sentinel for embeddings.max_batch_tokens, resolved
	// here rather than in Defaults() because Defaults() runs before the TOML and
	// env layers and so cannot tell "operator chose this value" from "operator
	// set nothing" — the same reason remote.known_hosts resolves after the
	// overlay. Resolution lives at the app layer so the config package needs no
	// /sys/fs/cgroup dependency.
	//
	// Auto-sizing clamps DOWN only: a small host or a memory-capped container
	// gets a smaller budget, but no machine ever raises the shipped default.
	// memlimit.Detect never fails — an undetectable ceiling yields the fixed
	// default, because embeddings are mandatory and must not be blocked by an
	// unknown amount of memory.
	lim := memlimit.Detect()
	budget := embeddings.ResolveBudget(cfg.Embeddings.MaxBatchTokens, lim)
	maxBatchTokens := budget.Tokens
	// Warn rather than reject: both bounds are judgement, not correctness.
	// The low warning catches a predictable operator error — the constant this
	// replaced was 32 DOCUMENTS, so someone reading a changelog may well set 32
	// here and get one max-length document per inference with no other signal.
	if n := cfg.Embeddings.MaxBatchTokens; n > 0 && n < 2048 {
		log.Warn().Int("max_batch_tokens", n).
			Msg("embeddings.max_batch_tokens is below one document's maximum length — the unit is PADDED TOKENS, not documents; every max-length document will run alone")
	}
	embedder, err := embeddings.NewEmbedder(ctx, model, filepath.Join(cfg.Home, "models"),
		embeddings.WithMaxBatchTokens(maxBatchTokens),
		embeddings.WithBatchConcurrency(budget.BatchConcurrency))
	if err != nil {
		return nil, fmt.Errorf("embedder init failed for model %q (embeddings are required — check ONNX model files / network): %w", model.ID, err)
	}
	a.closers = append(a.closers, embedder.Close)
	// The budget's provenance is logged, not just its value: a machine-derived
	// number with no explanation makes "why is re-embed slow HERE" unanswerable
	// without access to the box.
	log.Info().Str("model", model.ID).Int("dim", model.Dim).
		Int("max_batch_tokens", maxBatchTokens).
		Str("batch_budget_source", budget.Source).
		Str("batch_budget_clamped", budget.Clamped).
		Int64("memory_limit_bytes", budget.LimitBytes).
		Int("batch_concurrency", budget.BatchConcurrency).
		Msg("embedder enabled — facts indexed with vectors; semantic search and methodology vector ranking active")
	if budget.BatchConcurrency == 0 {
		// The one class with no memory bound at all. An operator on a host we
		// could not measure is exactly who needs telling, and a bare
		// batch_concurrency=0 field on the line above does not say it.
		log.Warn().Str("batch_budget_source", budget.Source).
			Msg("could not determine this host's memory ceiling, so concurrent embedding batches are UNBOUNDED and the batch budget is the shipped default; set embeddings.max_batch_tokens explicitly if this host is memory-constrained")
	}
	if budget.BatchConcurrency > 0 {
		log.Info().Int("max_batch_tokens", maxBatchTokens).
			Str("batch_budget_source", budget.Source).
			Int("batch_concurrency", budget.BatchConcurrency).
			Msg("concurrent embedding batches capped — this host's memory does not absorb unbounded overlap; interactive search is unaffected, since single-row inference bypasses the cap")
	}
	// The explicit-budget warning and the cap share one threshold deliberately.
	// Three separate thresholds on this axis previously left a band that was
	// modelled but neither warned nor bounded.
	if n := cfg.Embeddings.MaxBatchTokens; n > embeddings.DefaultMaxBatchTokens {
		log.Warn().Int("max_batch_tokens", n).
			Msg("embeddings.max_batch_tokens is above the shipped default; batch inference is serialized to compensate, and beyond 32768 tokens the memory cost is not covered by any measurement")
	}
	// FloorClass, not budget.Clamped: Clamped is always "none" for an explicit
	// budget, so an operator pinning a value on a small host would get no warning
	// at all — and for a cgroup source Clamped derives from a different fraction
	// and a different ceiling, so it answers a different question. The warning
	// should track the MACHINE, which is what FloorClass computes.
	if embeddings.FloorClass(lim) {
		log.Warn().Int("max_batch_tokens", maxBatchTokens).
			Str("batch_budget_source", budget.Source).
			Int64("memory_limit_bytes", budget.LimitBytes).
			Msg("this host has room for barely one full-length document per embedding inference; re-embedding will be slow and memory is the binding constraint")
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
