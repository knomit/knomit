package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#117b. The throttle told an operator three things it had not earned:
// a STATE (funded, on evidence that nothing had resolved), a DENOMINATOR (the
// throttleWindow constant, on a corpus with no verdicts at all), and a PROBE
// (spent on a pair that never reached the judge).
//
// The first two are display defects and the third is behavioural, so they are
// pinned separately below. Assertions read the RENDERED health line wherever
// the issue was filed on the rendered line — a restatementHealth field can be
// right while the line an operator reads is wrong, which is the whole family
// of bugs this file covers.

// throttleHealthLine returns the throttle line as the operator sees it.
func throttleHealthLine(sess *store.PipelineSession) string {
	for _, line := range sess.Health {
		if strings.HasPrefix(line, "shortlist throttle:") {
			return line
		}
	}
	return ""
}

// emittedHealthLine returns the emission line as the operator sees it.
func emittedHealthLine(sess *store.PipelineSession) string {
	for _, line := range sess.Health {
		if strings.HasPrefix(line, "restatement candidates emitted:") {
			return line
		}
	}
	return ""
}

func healthLineSaysProbing(sess *store.PipelineSession) bool {
	return strings.Contains(throttleHealthLine(sess), "(probing)")
}

func itemPairPaths(t *testing.T, factsJSON string) (string, string) {
	t.Helper()
	var facts []factForLLM
	require.NoError(t, json.Unmarshal([]byte(factsJSON), &facts))
	require.Len(t, facts, 2, "a shortlist work item carries exactly the pair")
	return facts[0].File, facts[1].File
}

// defund records enough all-keep verdicts to put the corpus below the floor,
// without going through selection — what defunds a corpus is the verdict
// history, and building it directly keeps the fixture independent of whatever
// selection happens to offer.
func defundCorpus(t *testing.T, env *restatementEnv) {
	t.Helper()
	for i := range throttleMinVerdicts {
		env.recordVerdict(fmt.Sprintf("kb/f%d.md", 2*i), fmt.Sprintf("kb/f%d.md", 2*i+1), false)
	}
}

// TestThrottle_UnprovenDoesNotGateSpending is the load-bearing half of the new
// state: `unproven` must be a DISPLAY state and nothing else.
//
// Only throttleDefunded is read by any branch, so a corpus that has judged a
// couple of pairs without resolving them must budget exactly what it budgeted
// before those verdicts existed. If `unproven` ever starts gating — the obvious
// wrong turn, since it looks like "half-defunded" — this is what catches it.
func TestThrottle_UnprovenDoesNotGateSpending(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()

	basePairs, baseHealth, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
	require.NoError(t, err)
	require.Equal(t, throttleOptimistic, baseHealth.ThrottleState, "no verdicts yet")

	// FIXTURE ASSERTION, not an assumption (the #130 vacuity rule): a corpus
	// budgeting one slot cannot distinguish "unrestricted" from "restricted to
	// a probe", and the comparison below would pass on both.
	require.Greater(t, len(basePairs), 1,
		"the fixture must budget more than a single slot or the comparison below is vacuous")

	// Judge some, resolve nothing, and stay BELOW the defund floor.
	judged := throttleMinVerdicts - 2
	require.Positive(t, judged)
	require.Less(t, judged, throttleMinVerdicts)
	for i := range judged {
		env.recordVerdict(basePairs[i].APath, basePairs[i].BPath, false)
	}

	pairs, health, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
	require.NoError(t, err)
	require.Equal(t, throttleUnproven, health.ThrottleState,
		"judged with nothing resolved, below the floor, is its own state")
	require.False(t, health.Probing, "an unproven corpus is not defunded and owes no probe")
	require.Equal(t, len(basePairs), len(pairs),
		"unproven must spend exactly what it spent before those verdicts — it is a DISPLAY state")
}

// TestHealthLine_ThrottleDenominatorIsWhatWasActuallyJudged pins the second
// display defect: the line printed the throttleWindow CONSTANT as its
// denominator, so a corpus that had never judged anything still reported
// "trailing resolution-rate 0% over last 10 judged". A rate is uninterpretable
// without its real denominator, and this one was decorative.
func TestHealthLine_ThrottleDenominatorIsWhatWasActuallyJudged(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
	require.NoError(t, err)
	require.NoError(t, planRestatementShortlist(ctx, env.deps(), sess, env.branch, nil))

	line := throttleHealthLine(sess)
	require.NotEmpty(t, line, "no throttle line rendered — every assertion below would be vacuous")
	require.Contains(t, line, "over last 0 judged", "nothing has been judged")
	require.NotContains(t, line, fmt.Sprintf("over last %d judged", throttleWindow),
		"the window constant is not a judged count")

	// And it tracks the REAL count rather than being pinned to zero: judge a
	// number that is neither 0 nor throttleWindow, so the assertion pins WHICH
	// value the denominator took, not merely that it moved (the X2 rule).
	pairs, _, err := selectRestatementCandidates(ctx, env.deps(), env.branch, nil, 600)
	require.NoError(t, err)
	judged := throttleMinVerdicts - 2
	require.GreaterOrEqual(t, len(pairs), judged, "fixture must offer enough pairs to judge")
	require.NotEqual(t, throttleWindow, judged)
	require.NotZero(t, judged)
	for i := range judged {
		env.recordVerdict(pairs[i].APath, pairs[i].BPath, false)
	}

	sess2, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
	require.NoError(t, err)
	require.NoError(t, planRestatementShortlist(ctx, env.deps(), sess2, env.branch, nil))
	require.Contains(t, throttleHealthLine(sess2), fmt.Sprintf("over last %d judged", judged))
}

// TestProbe_IsNotConsumedWhenTheSelectedPairIsNeverServed is the behavioural
// fix, and the one an existing test could not reach.
//
// TestDynamics_ProbeIsNotConsumedWhenNothingIsEmitted already covers the case
// where selection finds NOTHING to offer. The uncovered case is the one core
// actually hit: selection offers a pair, and the pair dies at item creation
// because a half no longer resolves (knomit#117a). Selection saw len(out) > 0
// and charged the probe, so a session that put nothing in front of a judge
// bought a full interval of silence — the self-defunding latch reintroduced at
// a slower rate, which is exactly what the probe exists to prevent.
func TestProbe_IsNotConsumedWhenTheSelectedPairIsNeverServed(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 600)
	env.seedShortlist()
	defundCorpus(t, env)

	// An unservable pair, ranked above anything this corpus produced on its
	// own: its B half is a fact id that does not resolve, so selection picks it
	// and enqueue drops it. A probe budgets exactly ONE slot, so out-ranking is
	// enough to guarantee it is the pair selected.
	//
	// Deliberately NOT done by clearing the other pairs: dropping every fact id
	// also marks those facts uncovered, and planRestatementShortlist refreshes
	// the cache before selecting, so the organic pairs come straight back.
	// Out-rank, do not clear.
	ids := env.liveFactIDs()
	require.NoError(t, env.svc.Abstraction().ReplaceRestatementPairs(ctx, env.branch, nil,
		[]store.RestatementPair{{
			APath: "kb/f100.md", BPath: "kb/gone.md",
			AFactID: ids["kb/f100.md"], BFactID: 999999,
			TitleCos: 0.999,
		}}, nil))

	// Run out the interval. Exactly one session arms a probe and selects the
	// unservable pair; it must NOT be recorded as having probed.
	dropped := 0
	for range throttleProbeInterval {
		sess, serr := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
		require.NoError(t, serr)
		require.NoError(t, planRestatementShortlist(ctx, env.deps(), sess, env.branch, nil))
		require.Empty(t, env.workItems(sess.ID), "nothing servable can reach the queue")
		if strings.Contains(emittedHealthLine(sess), "dropped unserved") {
			dropped++
			require.False(t, healthLineSaysProbing(sess),
				"a session that served nothing has not probed, whatever it selected")
		}
	}
	// CODE-REACHED assertion (the T4/V5 rule): without this the test passes on
	// a corpus that never armed a probe at all, and would still pass with the
	// consumption bug fully intact.
	require.Equal(t, 1, dropped,
		"exactly one session must have armed a probe and had its pair dropped")

	// The probe was never spent, so the corpus is still owed one: make a
	// SERVABLE pair available and the very next session must probe. Under the
	// old code the dropped session consumed the slot, so this session would be
	// silent and the corpus would wait another full interval for nothing.
	require.NoError(t, env.svc.Abstraction().ReplaceRestatementPairs(ctx, env.branch, nil,
		[]store.RestatementPair{{
			APath: "kb/f101.md", BPath: "kb/f102.md",
			AFactID: ids["kb/f101.md"], BFactID: ids["kb/f102.md"],
			TitleCos: 0.9999,
		}}, nil))

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch)
	require.NoError(t, err)
	require.NoError(t, planRestatementShortlist(ctx, env.deps(), sess, env.branch, nil))
	require.True(t, healthLineSaysProbing(sess),
		"the probe budget was still owed, not burned on a session that served nothing")
	require.Len(t, env.workItems(sess.ID), 1, "a probe is one slot")
}
