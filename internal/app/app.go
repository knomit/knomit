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

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/llm"
	"knomit/internal/platform/logging"
	"knomit/internal/platform/memlimit"
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
	// LogTap is the tap the caller wired into its own logging chain, served by
	// GET /api/v1/logs/events. Passed in rather than created here because the
	// logger is built before the app is — and in the desktop's case rebuilt on
	// every Settings save, over a tap that has to outlive each swap.
	//
	// nil is valid and means the endpoint answers 503: a binary that serves no
	// UI has no reason to carry a log ring.
	LogTap *logging.Tap
}

// New creates and boots the application from the given config and context.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	// First, before anything is opened or generated: a config that can only
	// produce a server refusing every request is refused here, at no cost.
	if err := checkLocalListener(cfg); err != nil {
		return nil, err
	}

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
		Auth:              cfg.Auth,
		SlowRequestMS:     cfg.Log.SlowRequestMS,
		Logs:              opts.LogTap,
		// 0 means experiments never expire, and the API then OMITS
		// expires_at rather than computing a date nobody set.
		ExperimentExpiryDays: cfg.Experiments.ExpiryDays,
	}

	// Start the manager (opens repos, launches background cluster
	// checker). Manager owns its own internal lifecycle — app does not
	// reach into checker config or stop hooks.
	if err := a.manager.Start(); err != nil {
		a.Close()
		return nil, fmt.Errorf("start manager: %w", err)
	}

	// Client-session recording. Set AFTER Start: the store is opened there,
	// over the control.db handle the repo registry owns, so reading it into
	// the server literal above would capture nil.
	a.server.ClientSessions = a.manager.ClientSessions()

	// Grants, like ClientSessions, lives in control.db and is set AFTER
	// Start for the same reason: the handle does not exist until then.
	sqlGrants := auth.NewSQLGrants(a.manager.ControlDB())
	a.server.Grants = sqlGrants

	// The OS user running the server is its operator, so seed the LOCAL
	// principal for our own account with [auth].loopback_default. That is what
	// makes the local bridge work with no configuration at all: it dials the
	// local listener, the OS says who is calling, and the grant is already
	// there.
	//
	// A failure here STOPS the boot rather than logging: a server that came
	// up without the seed would look healthy and refuse every local write,
	// which is harder to diagnose than not starting.
	if err := seedOwnPrincipal(ctx, sqlGrants, cfg.Auth); err != nil {
		a.Close()
		return nil, fmt.Errorf("seed grants: %w", err)
	}

	return a, nil
}

// checkLocalListener refuses [auth].require = true when no local
// authenticated listener is configured. Require turns the anonymous loopback
// path off, and in phase 1 the socket is the only credential there is, so
// such a server would boot, look healthy, and answer every request 403
// "Authentication required" -- a silent lockout. That is harder to diagnose
// than a server that did not start, the same argument that makes a
// seedOwnPrincipal failure stop the boot.
//
// It lives in app rather than config.Validate because it is a property of
// the SERVER boot: `kb` and other clients load the same config and have no
// business failing on a server-only combination. Both server boot paths
// (cmd/serve and the desktop app) reach New.
//
// IT CHECKS WHAT IS CONFIGURED, NOT WHAT IS BOUND, and the difference is a
// SECOND DOOR onto the same lockout that this check alone cannot close.
// auth.ListenLocal can return ErrSocketInUse — another process holds the
// path — which both callers treat as benign and serve TCP only for. With
// require = true that produces exactly the server this refuses at config
// time. auth.RequireLocalListener is the guard for it, called by cmd/serve.go
// and tools/desktop/boot.go straight after they listen; the two together are
// what the property actually rests on, and neither is sufficient alone.
//
// knomit#245 is what made that second door reachable: before it, Windows had
// no socket default, so require = true failed HERE and never got as far as
// listening.
func checkLocalListener(cfg config.Config) error {
	if cfg.Auth.Require && cfg.Socket == "" {
		return fmt.Errorf("[auth].require = true but no local authenticated listener is configured " +
			"(socket is empty): every request would be refused. Set socket, or set [auth].require = false")
	}
	return nil
}

// seedOwnPrincipal grants this process's own local principal each permission
// in loopback_default, ONCE per permission for the lifetime of the database.
//
// WHO we are comes from auth.LocalPrincipal and from nowhere else. The
// middleware names an incoming caller with auth.Peer.Principal, and the two
// are one formatting function per platform precisely so this seeded row and
// that request cannot disagree. They did disagree on Windows before
// knomit#245: os.Getuid() returns -1 there, so the boot seeded "uid:-1" and
// no request could ever have matched it.
//
// "Once" is the whole point, and it is why this asks EverGranted rather than
// For: a revoked row is history, not absence. Seeding on liveness would mean
// every restart silently undid an operator's revocation, which is the failure
// mode that makes a permission system worthless — the grant would be
// unrevokable in practice while appearing revocable.
func seedOwnPrincipal(ctx context.Context, g *auth.SQLGrants, cfg config.AuthConfig) error {
	set, err := auth.ParseSet(cfg.EffectiveLoopbackDefault())
	if err != nil {
		return err
	}
	me, err := auth.LocalPrincipal()
	if err != nil {
		return fmt.Errorf("resolve this machine's local principal: %w", err)
	}
	for perm := range set {
		ever, err := g.EverGranted(ctx, me, perm)
		if err != nil {
			return err
		}
		if ever {
			continue
		}
		if err := g.Grant(ctx, me, perm, "boot:loopback_default"); err != nil {
			return err
		}
	}
	return nil
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
