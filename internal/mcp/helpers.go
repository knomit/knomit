package mcp

import (
	"errors"

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
