package app

import (
	"knomit/internal/embeddings"
	"knomit/internal/memlimit"
)

// Fractions of the detected memory ceiling that knomit will claim for one
// embedding batch. Which one applies depends on WHICH SOURCE won, because the
// source implies the sharing model:
//
//   - A cgroup limit exists because an operator drew it around this workload,
//     so most of it is ours.
//   - Falling through to physical RAM means an unknown host with unknown
//     co-tenants. The 2026-08-29 OOM happened on exactly such a box, sharing
//     15.5 GiB with code-server and a coding agent.
//
// A single fraction for both cases is wrong in both directions: 0.8 of a shared
// desktop invites the original OOM, and 0.25 of a purpose-sized container
// wastes three quarters of what the operator provisioned.
const (
	dedicatedFraction = 0.80 // a cgroup limit was found
	sharedFraction    = 0.25 // fell through to physical RAM
)

// nonEmbeddingReserve is knomit's own footprint outside the embedder — Go heap,
// go-git, the SQLite index.
//
// This is a PLACEHOLDER, deliberately conservative. It cannot be read off a
// running server: the ONNX arena retains its high-water mark, so idle RSS
// equals peak RSS once any batch has run. The embedder now logs RSS immediately
// before its first inference (the only window where this is observable);
// replace this constant once those logs have been read across a few real boots.
const nonEmbeddingReserve = int64(1) << 30 // 1 GiB

// BatchBudget is a resolved budget plus the provenance an operator needs to
// understand it. A machine-derived number with no explanation is worse than a
// fixed one: "why is re-embed slow HERE" must be answerable from the log line.
type BatchBudget struct {
	Tokens int
	// Source is "explicit" when configured, otherwise the memlimit.Source that
	// won ("cgroup-v2", "cgroup-v1", "os-total", "unavailable").
	Source string
	// Clamped is "floor", "ceiling" or "none" — which bound, if any, the derived
	// value hit. "floor" is worth an operator's attention: it means the host is
	// small enough that a single full-length document fills a batch.
	Clamped string
	// LimitBytes is the detected ceiling, 0 when unknown.
	LimitBytes int64
	// Serialize gates batch inference behind a capacity-1 semaphore, making the
	// per-run budget a per-PROCESS bound.
	//
	// It is deliberately NOT always on, and the cost is NOT uniform. Measured on
	// an 8-core host (ratio = serialized/concurrent wall-clock):
	//
	//	4 workers x 4 rows x 2048  ->  54.66s vs 75.04s   ratio 1.37
	//	2 workers x 8 rows x 2048  ->  60.71s vs 61.78s   ratio 1.02
	//
	// A WIDE batch already saturates the cores through ORT's intra-op
	// parallelism, so serializing costs almost nothing. A NARROW one does not,
	// and concurrent Runs exploit the headroom — so serializing costs real
	// wall-clock.
	//
	// Be honest about where that leaves the gate: it fires on hosts whose memory
	// forced a SMALLER budget, and a smaller budget means NARROWER batches — the
	// expensive end of that range. The gate therefore costs most on the machines
	// where it engages. It is still right, because on those machines the memory
	// concurrency would spend is not there to spend: two overlapping runs are
	// what would OOM them, and an OOM costs more than 37%.
	//
	// On a roomy host — including the machine that motivated all of this, which
	// derives the ceiling exactly and survived 4.2 GiB of real overlap —
	// concurrency is kept and costs nothing.
	Serialize bool
}

// resolveMaxBatchTokens turns the configured value (0 meaning "auto") plus a
// detected memory ceiling into the budget to run with.
//
// Three rules, in order:
//
//  1. An explicit configured value is returned verbatim. Auto-sizing fills in
//     for silence only — overriding a set number would make the knob advisory.
//  2. An unknown ceiling yields the fixed default, never an error. Embeddings
//     are mandatory, so a strange sandbox must not be able to stop startup.
//  3. Otherwise derive, and clamp DOWN only. The result can never exceed
//     embeddings.DefaultMaxBatchTokens.
func resolveMaxBatchTokens(configured int, lim memlimit.Limit) BatchBudget {
	if configured > 0 {
		// An explicit value opts out of detection, and therefore out of the
		// serialization gate: the operator owns their number, and we have no
		// measurement of their machine to justify spending their throughput.
		return BatchBudget{Tokens: configured, Source: "explicit", Clamped: "none"}
	}
	if !lim.Known() {
		return BatchBudget{
			Tokens:  embeddings.DefaultMaxBatchTokens,
			Source:  string(memlimit.SourceNone),
			Clamped: "none",
		}
	}

	fraction := sharedFraction
	if lim.Source == memlimit.SourceCgroupV2 || lim.Source == memlimit.SourceCgroupV1 {
		fraction = dedicatedFraction
	}

	// Headroom for ONE batch: our share of the ceiling, less the resident model
	// and knomit's own non-embedding footprint. May go negative on a small
	// host, which BudgetForBatchMemory floors.
	avail := int64(float64(lim.Bytes)*fraction) - embeddings.ResidentModelBytes - nonEmbeddingReserve
	tokens := embeddings.BudgetForBatchMemory(avail)

	clamped := "none"
	switch tokens {
	case embeddings.MinBatchTokens:
		clamped = "floor"
	case embeddings.DefaultMaxBatchTokens:
		clamped = "ceiling"
	}

	return BatchBudget{
		Tokens:     tokens,
		Source:     string(lim.Source),
		Clamped:    clamped,
		LimitBytes: lim.Bytes,
		// Below the ceiling means the machine constrained us — see Serialize.
		Serialize: tokens < embeddings.DefaultMaxBatchTokens,
	}
}
