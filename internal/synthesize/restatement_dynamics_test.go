package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Multi-session dynamics.
//
// Every defect that made it past the first round of review lived here rather
// than in any single function: each component was correct on its own, and the
// system failed on the SECOND session, or the fifth, or the one after an edit.
// A unit test that calls a function once cannot see any of that.
//
// These tests therefore drive whole sessions in sequence — with partial
// backfill budgets, judge verdicts, and fact edits in between — and assert on
// what the corpus does over time.

// TestDynamics_IncrementalSessionsStillEmit is the regression for the defect
// that would have shipped: the budget was scaled by the session's DIRTY SEED
// count, not by corpus size. The first review of a repo full-scans, so seeds ≈
// corpus and everything looked right; every session after it sees a handful of
// changed facts, and a handful times five-per-thousand is zero. The feature
// would have emitted nothing for the entire life of every repo, with a full
// pair cache sitting unread.
func TestDynamics_IncrementalSessionsStillEmit(t *testing.T) {
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/restate-a.md", "Cache invalidation on write", "one account of it")
	env.writeFact("kb/restate-b.md", "Cache invalidation on write", "a different account, at length, of the same thing")
	for i := range 400 {
		env.writeFact(fmt.Sprintf("kb/filler%d.md", i), fmt.Sprintf("Filler %d", i), fmt.Sprintf("filler body %d", i))
	}

	// Session 1: the full scan. This is the session that always worked.
	first := env.runSession()
	require.NotEmpty(t, first.restatementItems, "the full-scan session emits")

	// Session 2: one fact edited, so the seed pool is a single fact.
	env.writeFact("kb/filler7.md", "Filler 7 revised", "a revised filler body")
	second := env.runSession()
	require.NotEmpty(t, second.restatementItems,
		"an incremental session must still spend from the CORPUS budget — "+
			"scaling by dirty seeds makes this zero forever after the first review")
	require.Contains(t, strings.Join(second.health, "\n"), "standing restatement pairs")
}

// TestDynamics_PartialBackfillClosesOverSessions is the regression for the
// frozen-cache defect. Facts were marked "covered" by the pair cache whether or
// not their titles had actually been embedded, so on any corpus too large to
// backfill in one session the un-embedded majority was recorded as done and
// their neighbours were never searched — permanently, because the cache state
// is exactly what says "already covered".
func TestDynamics_PartialBackfillClosesOverSessions(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 120)
	env.emb.perBatchDelay = 15 * time.Millisecond // forces a partial first pass

	// A deliberately tiny backfill budget: a few batches per session.
	const tinyBudget = 40 * time.Millisecond

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, tinyBudget)
	require.NoError(t, err)
	require.Less(t, have, total, "the first session covers only part of the corpus")

	_, err = refreshRestatementShortlist(ctx, env.deps(), env.branch)
	require.NoError(t, err)
	firstPairs := env.standingPairCount()

	// Sessions continue until coverage closes. Pair growth must continue with
	// it: a frozen cache would keep returning the same count forever.
	for range 40 {
		have, total, err = ensureTitleVectors(ctx, env.deps(), env.branch, tinyBudget)
		require.NoError(t, err)
		_, err = refreshRestatementShortlist(ctx, env.deps(), env.branch)
		require.NoError(t, err)
		if have == total {
			break
		}
	}
	require.Equal(t, total, have, "coverage closes")
	require.Greater(t, env.standingPairCount(), firstPairs,
		"pairs keep being discovered as the axis fills — a cache that stops growing "+
			"has marked un-embedded facts as covered")

	// And the finished corpus matches what a single-session backfill would have
	// produced: partial coverage delays discovery, it does not lose it.
	full := newRestatementEnv(t, 120)
	full.seedShortlist()
	require.Equal(t, full.standingPairCount(), env.standingPairCount(),
		"a corpus backfilled across sessions ends up with the same pairs as one backfilled at once")
}

// TestDynamics_PairsSurviveAnUnrelatedEdit is the regression for asymmetric-KNN
// delta loss. A standing pair can exist only because B's top-K included A and
// never the reverse; dropping every pair that touches an edited fact and then
// re-running only that fact's KNN destroys such pairs with nothing to
// rediscover them.
func TestDynamics_PairsSurviveAnUnrelatedEdit(t *testing.T) {
	env := newRestatementEnv(t, 60)
	env.seedShortlist()
	before := env.standingPairSet()
	require.NotEmpty(t, before)

	// Edit facts one at a time, refreshing between each, and require the pair
	// population never to erode. Each edit legitimately retires the pairs of the
	// edited fact and re-mints them; what must not happen is a net loss of pairs
	// between OTHER facts.
	for _, i := range []int{3, 17, 42} {
		path := fmt.Sprintf("kb/f%d.md", i)
		env.writeFact(path, fmt.Sprintf("F%d", i), fmt.Sprintf("rewritten body %d", i))
		env.seedShortlist()

		after := env.standingPairSet()
		for key := range before {
			if strings.Contains(key, path) {
				continue // the edited fact's own pairs are re-minted, not preserved
			}
			require.Contains(t, after, key,
				"pair %s disappeared after editing %s — an asymmetric KNN discovery was lost", key, path)
		}
		before = after
	}
}

// TestDynamics_DefundAndProbeRecoveryOverSessions walks the throttle's whole
// life cycle through the paths production actually takes: decline until the
// corpus defunds itself, wait out the probe interval, spend the probe, resolve
// it, and be funded again.
//
// The earlier version of this suite "restored" funding by writing a resolution
// verdict directly — a state a defunded corpus can never reach on its own,
// which is exactly why the latch went unnoticed.
func TestDynamics_DefundAndProbeRecoveryOverSessions(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()

	// Decline everything offered until the corpus stops offering. How many
	// sessions that takes depends on the batch size, which is the corpus's
	// business, not this test's — what matters is that it happens, bounded.
	declined := map[string]bool{}
	var health restatementHealth
	var pairs []store.RestatementPair
	var err error
	sessions := 0
	for ; sessions < throttleWindow; sessions++ {
		pairs, health, err = selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		if health.ThrottleState == throttleDefunded {
			break
		}
		require.NotEmpty(t, pairs, "a funded corpus keeps offering work")
		for _, p := range pairs {
			key := pathPairKey(p.APath, p.BPath)
			require.False(t, declined[key], "a declined pair must never be offered twice")
			declined[key] = true
			env.recordVerdict(p.APath, p.BPath, false)
		}
	}
	require.Equal(t, throttleDefunded, health.ThrottleState,
		"declining everything must eventually stop the spending")
	require.Empty(t, pairs)
	require.GreaterOrEqual(t, len(declined), throttleMinVerdicts)

	// Quiet sessions, then exactly one probe.
	probed := 0
	var probePair store.RestatementPair
	for range throttleProbeInterval + 1 {
		pairs, health, err = selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		if health.Probing {
			probed++
			require.Len(t, pairs, 1, "a probe is one slot, not a batch")
			probePair = pairs[0]
		} else {
			require.Empty(t, pairs, "a defunded corpus is silent between probes")
		}
	}
	require.Equal(t, 1, probed, "exactly one probe per interval")

	// Resolve the probe: the corpus funds itself again from evidence only the
	// probe could have produced.
	env.recordVerdict(probePair.APath, probePair.BPath, true)
	pairs, health, err = selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
	require.NoError(t, err)
	require.Equal(t, throttleFunded, health.ThrottleState)
	require.NotEmpty(t, pairs)
}

// TestDynamics_DecliningSessionsKeepOfferingNewPairs is the regression for
// window saturation: with declined pairs left standing at the top of the
// ranking, a handful of declining sessions filled the entire selection window
// with pairs the judge had already refused, and a funded corpus went silent.
func TestDynamics_DecliningSessionsKeepOfferingNewPairs(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()

	seen := map[string]bool{}
	for session := range throttleMinVerdicts - 1 {
		pairs, _, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		require.NotEmpty(t, pairs, "session %d went silent while still funded", session)
		for _, p := range pairs {
			key := pathPairKey(p.APath, p.BPath)
			require.False(t, seen[key], "session %d re-offered %s", session, key)
			seen[key] = true
			// Decline, but resolve one so the corpus stays funded throughout.
			env.recordVerdict(p.APath, p.BPath, session == 0)
		}
	}
	require.GreaterOrEqual(t, len(seen), throttleMinVerdicts-1)
}

// ── session harness ───────────────────────────────────────────────────────

type sessionOutcome struct {
	sessionID        string
	restatementItems []store.PipelineWorkItem
	health           []string
}

// runSession drives one real review session through the engine and reports what
// the shortlist contributed to it.
func (e *restatementEnv) runSession() sessionOutcome {
	e.t.Helper()
	res, err := NewReviewerWithOptions(e.ri, nil, EffortNormal, ScopeFilter{}).StartSession(context.Background())
	require.NoError(e.t, err)

	out := sessionOutcome{sessionID: res.SessionID, health: res.Health}
	for _, item := range e.workItems(res.SessionID) {
		if strings.HasPrefix(item.ClusterKey, restatementClusterKeyPrefix) {
			out.restatementItems = append(out.restatementItems, item)
		}
	}
	return out
}

func (e *restatementEnv) standingPairCount() int {
	e.t.Helper()
	stats, err := e.svc.Abstraction().RestatementPairStats(context.Background(), e.branch)
	require.NoError(e.t, err)
	return stats.Count
}

func (e *restatementEnv) standingPairSet() map[string]bool {
	e.t.Helper()
	pairs, err := e.svc.Abstraction().RestatementPairsByRank(context.Background(), e.branch, 100_000)
	require.NoError(e.t, err)
	out := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		out[pathPairKey(p.APath, p.BPath)] = true
	}
	return out
}

// TestDynamics_ProbeIsNotConsumedWhenNothingIsEmitted — the probe is the only
// path by which a defunded corpus can change its own evidence, so spending one
// on a session that emits nothing costs a full interval of silence for no
// information.
//
// This is the repair's own recursion: the probe fix restored recovery, and this
// asks what the repair does when the thing it funds turns out to be empty.
func TestDynamics_ProbeIsNotConsumedWhenNothingIsEmitted(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()

	// Decline until defunded.
	for range throttleWindow {
		pairs, health, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		if health.ThrottleState == throttleDefunded {
			break
		}
		for _, p := range pairs {
			env.recordVerdict(p.APath, p.BPath, false)
		}
	}

	// Retire every remaining standing pair, so a probe has nothing to offer.
	standing, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100_000)
	require.NoError(t, err)
	for _, p := range standing {
		require.NoError(t, env.svc.Abstraction().DeleteRestatementPair(ctx, env.branch, p.AFactID, p.BFactID))
	}

	// Run well past a probe interval. No pair can be emitted, so no probe may
	// be marked as spent.
	for range throttleProbeInterval * 2 {
		pairs, health, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		require.Empty(t, pairs)
		require.False(t, health.Probing, "a session that emits nothing has not probed")
	}

	// Put the pairs back. Re-running the refresh alone would not do it — every
	// fact is already marked covered — so evict the cache state the way a
	// partner requeue does, which forces a rescan.
	ids := env.liveFactIDs()
	all := make([]int64, 0, len(ids))
	for _, id := range ids {
		all = append(all, id)
	}
	require.NoError(t, env.svc.Abstraction().ReplaceRestatementPairs(ctx, env.branch, all, nil, nil))
	env.seedShortlist()
	require.Positive(t, env.standingPairCount(), "the corpus has pairs to offer again")
	probed := false
	for range throttleProbeInterval {
		pairs, health, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
		require.NoError(t, err)
		if health.Probing {
			probed = true
			require.NotEmpty(t, pairs)
			break
		}
	}
	require.True(t, probed, "the probe budget was still owed, not burned on empty sessions")
}
