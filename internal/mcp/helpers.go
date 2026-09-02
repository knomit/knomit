package mcp

import (
	"context"
	"errors"

	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// errStoreUnavailable is the caller-facing error for a mount whose store
// cannot be acquired right now (repo closed, mid-SwapStore, or still opening).
// The text is written for the LLM caller: actionable, no internal detail.
var errStoreUnavailable = errors.New("knowledge base is unavailable (repo is closed, replacing its store, or still opening) — retry shortly")

// mcpStore bundles the store indices used by MCP handlers. The query surface is
// split by cluster: MCP uses FactQuery, GraphStore, and HistoryQuery, never
// MethodologyMatcher.
type mcpStore struct {
	facts       store.FactIndex
	factQuery   store.FactQuery
	graph       store.GraphStore
	history     store.HistoryQuery
	toolSession store.ToolSessionIndex
	pipeline    store.PipelineIndex
	branches    store.BranchIndex
	// motifs is alias resolution over the motif vocabulary — read-only here.
	// The §6 explain surface resolves a fact's motifs to their cluster,
	// definition and siblings; nothing on this path writes derived state.
	motifs store.MotifIndex
}

// storeIndices acquires the mount's store and returns its indices plus the
// release func the caller MUST invoke when done with them (defer it, or — in
// fan-out goroutines — call it before the goroutine exits). Holding the
// acquisition, rather than copying pointers out of a lock, is what keeps a
// concurrent SwapStore/Archive on this mount from closing the SQLite handle
// mid-call: the repos layer drains acquirers before closing (RepoInstance
// lifetime contract). On error the release is a no-op and the indices are
// zero; callers surface errStoreUnavailable to the LLM.
func storeIndices(ri *repos.RepoInstance) (mcpStore, func(), error) {
	svc, release, err := ri.Acquire()
	if err != nil {
		return mcpStore{}, func() {}, errStoreUnavailable
	}
	return mcpStore{
		facts:       svc.Facts(),
		factQuery:   svc.FactQuery(),
		graph:       svc.GraphStore(),
		history:     svc.HistoryQuery(),
		toolSession: svc.ToolSession(),
		pipeline:    svc.Pipeline(),
		branches:    svc.Branches(),
		motifs:      svc.Motifs(),
	}, release, nil
}

// fanoutQueryVec embeds a federated query's text ONCE, for every mount in the
// fan-out to share, and is the reason a lens query is not N inferences of the
// same string.
//
// Without it the cost is real and it is the whole request: store.Search embeds
// whenever SearchOptions.QueryVec is empty (internal/store/search_query.go), so
// an N-mount fan-out ran the identical ~81 ms ONNX inference N times. It is a
// fixed per-mount cost, independent of corpus size — the same query against an
// EMPTY mount also cost 81 ms, while that mount answered a text-less filter in
// 0.4 ms. A single-repo binding never showed it: there is only one mount to pay
// for.
//
// The hoist cannot change a result. repos.Manager installs ONE embedder on
// every repo instance, so the N vectors were already identical by construction
// — this computes one of them instead of N.
//
// A nil vector is the DEGRADED path, not an error: with no embedder, no text,
// or a failed inference, each mount falls back to exactly what it does today
// (its own embedder, else keyword-only search). Never fail a query because it
// could not be embedded.
func fanoutQueryVec(ctx context.Context, emb store.Embedder, text string) []float32 {
	if text == "" || emb == nil {
		return nil
	}
	vec, err := emb.EmbedQuery(ctx, text)
	if err != nil {
		log.Warn().Err(err).Msg("federated query: embed query failed")
		return nil
	}
	return vec
}
