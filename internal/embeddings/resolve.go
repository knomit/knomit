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
	// BatchConcurrency caps how many BATCH inferences may run at once. 0 means
	// unbounded. See ResolveBudget for how the value is chosen per machine class.
	//
	// Scope precisely: this bounds concurrent BATCH memory, NOT the process.
	// EmbedQuery and EmbedDocument bypass the gate by design (so interactive
	// search never queues behind a rebuild), and each is up to one MaxTokens row
	// — ~225 MiB measured — retained by the same arena. A busy single-shot path
	// is therefore unbounded in count and outside this guarantee.
	BatchConcurrency int
}

// Serialized reports whether batch inference is fully serialized.
func (b Budget) Serialized() bool { return b.BatchConcurrency == 1 }

// WorstCaseBatchBytes reports the measured worst-case memory for one batch at
// the given budget, above the resident model.
//
// ONLY MEANINGFUL UP TO maxLadderTokens. Above that the ladder has no data and
// this returns the top rung, which UNDERSTATES the real cost — the curve is
// superlinear. Callers making a safety decision must treat tokens >
// maxLadderTokens as unmodelled rather than as "costs the same as the top
// rung"; shouldSerialize does exactly that. Saturating quietly is the one
// behaviour that would be wrong in the unsafe direction, which is why this
// warning is here rather than in a commit message.
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
//   - A CGROUP limit ALWAYS serializes (capacity 1). It is a hard wall with no
//     absorber behind it, and any K-run threshold is beatable by K+1 concurrent
//     branches, so a threshold there moves the cliff rather than removing it.
//   - A FLOOR-CLASS physical-RAM host serializes (capacity 1): our share of it
//     cannot fund even the smallest budget's batch (7.52 GiB boundary).
//   - A ROOMIER physical-RAM host gets capacity 2 rather than no bound at all.
//     Concurrent peaks ADD and the arena RETAINS the high-water mark, so an
//     unlucky 3-way overlap does not spike and recover — it raises resting RSS
//     for the process lifetime. On the deployed 15.9 GiB machine, 3-way overlap
//     is 5460 MiB of batch against the 3.97 GiB that the fraction claims is
//     knomit's entire share of the box.
//     K=2 is grounded in inputs INDEPENDENT of the sizing chain, which is the
//     discipline a previous tautological gate here failed: that machine was
//     OBSERVED absorbing ~2 runs' worth (4025 MiB peak across three repos), and
//     2 workers is where the measured concurrency win still lives. It does not
//     change the common case — 2-way overlap is untouched; only a third
//     simultaneous batch queues.
//   - An UNREADABLE ceiling serializes: we know a limit may apply and could not
//     read it, which is the one case where we know we are blind.
//   - An UNKNOWN ceiling is UNBOUNDED. We measured nothing, so we know nothing,
//     and imposing a cost on an unmeasured host is unfounded. Stated openly
//     rather than hidden: this is the one class with no memory bound at all.
//   - An EXPLICIT budget above DefaultMaxBatchTokens serializes AND warns. The
//     operator has overridden our sizing, so they are least likely to have
//     modelled overlap, and beyond the ladder we cannot model it either. The
//     guard sits where the DANGER starts, not where the measurements stop.
//
// Apart from that last case the bound is independent of where Tokens came from.
// An explicit value sets the budget; it is not a concurrency policy, and an
// operator who pins a budget inside a container must not silently lose the
// memory guarantee. If concurrency-despite-a-cgroup is ever genuinely wanted,
// that is a separate config knob with its own name, added when someone asks.
func ResolveBudget(configured int, lim memlimit.Limit) Budget {
	b := Budget{Tokens: configured, Source: "explicit", Clamped: "none", LimitBytes: lim.Bytes}

	if configured <= 0 {
		switch {
		case !lim.Known():
			// Source is carried through rather than flattened to "unavailable":
			// "unreadable" means we know a limit may apply and could not read it,
			// which is what an operator needs to see in the boot log to know an
			// explicit budget is worth setting.
			b = Budget{Tokens: DefaultMaxBatchTokens, Source: string(lim.Source), Clamped: "none"}
			if lim.Source == "" {
				b.Source = string(memlimit.SourceNone)
			}
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

	b.BatchConcurrency = batchConcurrency(lim, b.Tokens)
	return b
}

func isCgroup(s memlimit.Source) bool {
	return s == memlimit.SourceCgroupV2 || s == memlimit.SourceCgroupV1
}

// batchConcurrency returns the cap on simultaneous batch inferences, 0 for
// unbounded. See ResolveBudget for the rule and for why it is not "did we clamp".
func batchConcurrency(lim memlimit.Limit, tokens int) int {
	switch {
	case tokens > DefaultMaxBatchTokens:
		// Only reachable via an explicit operator value — derivation never
		// exceeds the default. Above the ladder we cannot model the memory at
		// all, and between the default and the ladder's end we can model it but
		// the operator has overridden the sizing that would have bounded it.
		// Both want the same answer, so there is one threshold rather than three.
		return 1
	case isCgroup(lim.Source):
		return 1
	case lim.Source == memlimit.SourceUnreadable:
		// We POSITIVELY KNOW a limit may apply and could not read it. The same
		// reasoning that stops detection reporting physical RAM here applies to
		// concurrency: falling back to the default budget AND permitting
		// unbounded overlap would be behaviourally identical to the over-report
		// this case exists to prevent, with only the log line changed. This is
		// the one situation where we know we are blind, so it is the one where
		// guessing is least defensible.
		return 1
	case lim.Source == memlimit.SourceOSTotal && lim.HostTotal > 0:
		// FLOOR-CLASS MACHINE: our share of this host cannot fund even the
		// smallest budget's batch. Computed from the machine alone — HostTotal
		// and the constants, never the resolved Tokens — so it is independent of
		// whether the budget was derived or configured. An operator pinning a
		// budget on a small laptop must not silently lose the guarantee.
		//
		// Note this applies sharedFraction, i.e. the SAME envelope the budget
		// math uses. An earlier form asked the question of raw HostTotal on the
		// theory that unclaimed RAM absorbs overlap. That was over-read from an
		// observation about the incident machine: its unclaimed ~12 GiB was not
		// idle, it held code-server and a coding agent, and it absorbed the
		// overlap because the overlap was small relative to the box, not because
		// the memory was free. Using two different envelopes also made the app
		// self-contradictory — on a 4 GiB host it warned that memory was the
		// binding constraint while permitting unbounded overlap.
		//
		// Keying on "floor-class" is a proxy, and the corpus says to check a
		// proxy's correlation direction per source before trusting it. Here it
		// is POSITIVELY correlated with constraint: no fraction is spent on a
		// first batch before the comparison, so a smaller machine always means a
		// smaller share. That is the opposite of the cgroup path, where the
		// analogous proxy was ANTI-correlated and produced an OOM window.
		//
		// Boundary is 7696 MiB of physical RAM (7.52 GiB), from
		// 0.25*H <= 225 + 675 + 1024 MiB.
		share := int64(float64(lim.HostTotal)*sharedFraction) - ResidentModelBytes - nonEmbeddingReserve
		if share <= WorstCaseBatchBytes(MinBatchTokens) {
			return 1
		}
		// Roomier, but not unbounded — see ResolveBudget for K=2's grounding.
		return roomyHostConcurrency
	default:
		// Unknown ceiling: no bound, stated openly rather than hidden.
		return 0
	}
}

// roomyHostConcurrency caps simultaneous batches on a physical-RAM host that is
// above floor-class. Not derived from the budget ladder, deliberately: a
// previous gate here compared two quantities that were exact inverses of each
// other and was therefore unsatisfiable. This value comes from two independent
// observations — the deployed machine absorbed ~2 runs' worth under real load
// (4025 MiB across three concurrent repos), and 2 workers is where the measured
// concurrency benefit still lives.
const roomyHostConcurrency = 2
