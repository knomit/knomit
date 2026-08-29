package embeddings

import "knomit/internal/memlimit"

// Fractions of the detected ceiling that knomit claims for one embedding batch.
// These size the BUDGET only — since decision 11 the safety question (whether
// to serialize) is answered from the real memory envelope instead, so a
// generous fraction can no longer quietly erode the memory guarantee.
//
// Which one applies depends on where the limit was found, because that is what
// the fraction's premise actually claims:
//
//   - dedicatedFraction: the limit is on OUR cgroup. An operator drew it around
//     this workload, so most of it is ours.
//   - sharedFraction: the limit came from an ANCESTOR cgroup, or from physical
//     RAM. Both mean "unknown co-tenants" — an ancestor limit was drawn around a
//     GROUP we merely belong to, and physical RAM is shared with everything by
//     definition. The 2026-08-29 OOM happened on such a box, sharing 15.5 GiB
//     with code-server and a coding agent.
//
// The distinction is load-bearing because cgroupV2Limit takes the MINIMUM OVER
// ANCESTORS: without it, an operator capping a slice knomit merely sits inside
// would move knomit from claiming 0.25 of physical RAM to claiming 0.8 of that
// slice — i.e. adding a protective limit would make knomit MORE aggressive
// against LESS memory.
const (
	dedicatedFraction = 0.80
	sharedFraction    = 0.25
)

// nonEmbeddingReserve is knomit's own footprint outside the embedder — Go heap,
// go-git, the SQLite index.
//
// A PLACEHOLDER, deliberately conservative. It cannot be read off a running
// server: the ONNX arena retains its high-water mark, so idle RSS equals peak
// RSS once any batch has run. The embedder logs RSS immediately before its first
// inference (the only window where this is observable); replace this once those
// logs have been read across a few real boots.
const nonEmbeddingReserve = int64(1) << 30

// expectedConcurrentRuns is how many inferences the safety check assumes may
// overlap when the gate is off. Two, because that is what was observed in
// production: a rebuild on one branch concurrent with a learn on another.
const expectedConcurrentRuns = 2

// Budget is a resolved batch budget plus the provenance an operator needs to
// understand it. A machine-derived number with no explanation makes "why is
// re-embed slow HERE" unanswerable without access to the box.
type Budget struct {
	Tokens int
	// Source is "explicit" when configured, otherwise the memlimit.Source that
	// won ("cgroup-v2", "cgroup-v1", "os-total", "unavailable").
	Source string
	// Clamped is "floor", "ceiling" or "none".
	Clamped string
	// LimitBytes is the detected ceiling, 0 when unknown.
	LimitBytes int64
	// Serialize gates batch inference behind a capacity-1 semaphore, making the
	// per-run budget a per-PROCESS bound. See ResolveBudget for the rule.
	Serialize bool
}

// WorstCaseBatchBytes reports the measured worst-case memory for one batch at
// the given budget, above the resident model.
func WorstCaseBatchBytes(tokens int) int64 {
	if tokens <= budgetLadder[0].tokens {
		return budgetLadder[0].bytes
	}
	for i := 1; i < len(budgetLadder); i++ {
		lo, hi := budgetLadder[i-1], budgetLadder[i]
		if tokens <= hi.tokens {
			span := int64(hi.tokens - lo.tokens)
			return lo.bytes + (hi.bytes-lo.bytes)*int64(tokens-lo.tokens)/span
		}
	}
	return budgetLadder[len(budgetLadder)-1].bytes
}

// ResolveBudget turns the configured value (0 meaning "auto") plus a detected
// memory ceiling into the budget and concurrency policy to run with.
//
// THE BUDGET. An explicit value is used verbatim — auto-sizing fills in for
// silence only. An unknown ceiling yields the fixed default, never an error,
// because embeddings are mandatory and a strange sandbox must not stop startup.
// Otherwise derive, clamping DOWN only.
//
// SERIALIZATION keys on the DEATH BOUNDARY, and deliberately not on whether the
// budget was clamped. That earlier rule contained a live OOM window: a 4400 MiB
// cgroup derived a budget sized for exactly one run with 1 MiB of slack, and
// then — because the budget happened to reach the ceiling — switched the gate
// off, so adding 100 MiB to a 4300 MiB container REMOVED the per-process bound.
// The units were wrong: it asked "did we clamp?" when the property that must
// hold is "can this ceiling absorb concurrent peaks".
//
//   - A CGROUP limit ALWAYS serializes. It is a hard wall with no absorber
//     behind it, and any K-run threshold is beatable by K+1 concurrent branches,
//     so a threshold there moves the cliff rather than removing it. The cost is
//     width-dependent (~2% at the wide shapes a container running a big rebuild
//     actually hits, more at narrow ones) and is the price of the guarantee.
//   - PHYSICAL RAM serializes only when the real envelope cannot absorb
//     expectedConcurrentRuns. Note this uses HostTotal, NOT the fraction: the
//     deployed 15.9 GiB machine's safety came from the ~75% it never claimed
//     being a genuine absorber, so the safety question must be asked of the real
//     memory rather than of our share of it.
//   - An UNKNOWN ceiling does not serialize. We measured nothing, so we know
//     nothing; an unmeasured host is not a constrained one.
//
// Serialization is independent of where Tokens came from. An explicit value
// sets the budget; it is not a concurrency policy, and an operator who pins a
// budget inside a container must not silently lose the memory guarantee. If
// concurrency-despite-a-cgroup is ever genuinely wanted, that is a separate
// config knob with its own name, added when someone asks for it.
func ResolveBudget(configured int, lim memlimit.Limit) Budget {
	b := Budget{Tokens: configured, Source: "explicit", Clamped: "none", LimitBytes: lim.Bytes}

	if configured <= 0 {
		switch {
		case !lim.Known():
			b = Budget{Tokens: DefaultMaxBatchTokens, Source: string(memlimit.SourceNone), Clamped: "none"}
		default:
			fraction := sharedFraction
			if isCgroup(lim.Source) && !lim.Inherited {
				fraction = dedicatedFraction
			}
			avail := int64(float64(lim.Bytes)*fraction) - ResidentModelBytes - nonEmbeddingReserve
			tokens := BudgetForBatchMemory(avail)

			clamped := "none"
			switch tokens {
			case MinBatchTokens:
				clamped = "floor"
			case DefaultMaxBatchTokens:
				clamped = "ceiling"
			}
			b = Budget{Tokens: tokens, Source: string(lim.Source), Clamped: clamped, LimitBytes: lim.Bytes}
		}
	}

	b.Serialize = shouldSerialize(lim, b.Tokens)
	return b
}

func isCgroup(s memlimit.Source) bool {
	return s == memlimit.SourceCgroupV2 || s == memlimit.SourceCgroupV1
}

// shouldSerialize answers "can this machine survive expectedConcurrentRuns
// overlapping batches" — see ResolveBudget for why that, and not "did we clamp".
func shouldSerialize(lim memlimit.Limit, tokens int) bool {
	switch {
	case isCgroup(lim.Source):
		return true
	case lim.Source == memlimit.SourceOSTotal && lim.HostTotal > 0:
		absorber := lim.HostTotal - ResidentModelBytes - nonEmbeddingReserve
		return absorber < int64(expectedConcurrentRuns)*WorstCaseBatchBytes(tokens)
	default:
		return false
	}
}
