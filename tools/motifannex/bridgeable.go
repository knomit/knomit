package main

// bridgeablePairs returns how many fact pairs share a canonical motif at all,
// given each cluster's df.
//
// WHAT READS THIS NUMBER, AND WHAT IT MEANS. Phase 4's activation floor is set
// on it (Q1 ruling), and the gate annex's "starved, not broken" finding is a
// hand computation of it: knomit-io-kb reached 100% coverage and a saturated
// 22-cluster vocabulary, and still offered exactly TWO candidate pairs — "two
// shots is not a sample".
//
// It is a CEILING and nothing more. Every gate the engine applies — df band,
// subject-disjointness, separation, cohesion, the member cap, the per-lane
// budgets — can only take pairs away from this number. So a corpus with a high
// count may still bridge nothing, and reading it as "pairs the agent will see"
// is wrong in the direction that flatters the axis. What it rules out is the
// other direction: below it, no mechanism can find anything, because there is
// nothing of the right shape to find.
//
// Hapax contribute zero by construction, which is the same reason the df band's
// floor is 2: a motif carried by one fact joins nothing to anything.
//
// ONE PLACE THE LABEL COULD DRIFT FROM THE NUMBER. This sums C(df,2) per
// cluster, so it counts CLUSTER-PAIR INCIDENCES. A reader takes it as DISTINCT
// FACT PAIRS, and the two differ as soon as one pair of facts shares two
// motifs. Measured 2026-08-24 on the lab corpora, they do not differ yet
// (agentic-engineering 8 = 8, merged 16 = 16) — but "equal today" is not
// "the same quantity", and a corpus with a richer vocabulary will separate
// them. Whoever needs distinct fact pairs must count them, not read this.
//
// CORRECTION TO A PUBLISHED FIGURE. The gate annex §9 states this ceiling as
// 7 on agentic-engineering. It is 8: the corpus carries one df-3 cluster
// (instrument-fault-mimics-signal) and five df-2 clusters, giving 3 + 5. The
// annex's own §4 table lists the 6 recurring clusters this is computed from,
// so the slip is in the arithmetic, not the data. It changes no conclusion —
// 8 is as starved as 7 — and is recorded because a number nobody re-derived
// is how the annex's own §11 item 4 happened.
func bridgeablePairs(dfs []int) int {
	total := 0
	for _, df := range dfs {
		if df < 2 {
			continue
		}
		total += df * (df - 1) / 2
	}
	return total
}
