package synthesize

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The review pipeline's half of motif bridging, driven through the REAL engine
// rather than by calling the builder directly. What these tests are for is the
// seam: the builder can be right, the payload can be right, and the item can
// still reach the agent with the wrong direction on it.

// motifBridgeSession runs one real review session at the given effort and
// returns the discover payloads it enqueued, plus the session's health lines.
func (e *restatementEnv) motifBridgeSession(effort Effort) ([]DiscoverWorkPayload, []string) {
	e.t.Helper()
	res, err := NewReviewerWithOptions(e.ri, nil, effort, ScopeFilter{}).StartSession(context.Background())
	require.NoError(e.t, err)

	var out []DiscoverWorkPayload
	for _, item := range e.workItems(res.SessionID) {
		if item.StepType != "discover" {
			continue
		}
		var p DiscoverWorkPayload
		require.NoError(e.t, json.Unmarshal([]byte(item.FactsJSON), &p))
		out = append(out, p)
	}
	return out, res.Health
}

// motifPayloads filters to the motif axis.
func motifPayloads(in []DiscoverWorkPayload) []DiscoverWorkPayload {
	var out []DiscoverWorkPayload
	for _, p := range in {
		if p.Bridge.Kind == BridgeMotif {
			out = append(out, p)
		}
	}
	return out
}

// seedMotifPair writes two subject-disjoint facts carrying the same motif. They
// have distinct paths, distinct titles and distinct bodies, so nothing but the
// motif connects them — which is the whole claim the axis makes.
func (e *restatementEnv) seedMotifPair(motif string) {
	e.t.Helper()
	// The corpus needs enough RECURRING vocabulary to clear the activation
	// floor before any of it enumerates (phase4-rulings-4). These fillers each
	// share an entity within their pair, so disjointness rejects them and they
	// never become candidates — they move the activation count and nothing
	// else, which is what keeps every "one discover item" assertion below
	// meaning what it says.
	e.seedActivationVocabulary()
	// rewriteWithMotifs, NOT writeFactWithMotifs: the latter stamps
	// `domain: [alpha]` on every fact it writes, so a "subject-disjoint" pair
	// built with it shares a domain tag. That went unnoticed while the umbrella
	// exclusion was degenerate at this corpus size and swallowed the shared tag
	// (Phase-3 review, L4) — the fixture was never disjoint, and the gate was
	// never the reason it passed.
	e.rewriteWithMotifs("kb/gotchas/uitesting/agentclicks.md",
		"An agent testing a UI will execute JavaScript instead of clicking",
		"Driving app state directly bypasses the path the verifier believes it is checking.",
		[]string{motif}, []string{"Cognition"})
	e.rewriteWithMotifs("kb/technology/benchmarks/terminalscores.md",
		"Coding agents reached 94% on a terminal benchmark by cheating it",
		"The agents scored by exploiting the harness rather than by solving the tasks.",
		[]string{motif}, []string{"Terminal Bench"})
}

// TestReviewPlan_FarLaneRoutesBackward — the far lane's members have no
// SIMILAR_TO edge, so the item must carry DiscoverBackward, which is what makes
// the proposal a hypothesis under the blast-radius gate (2bc84184).
func TestReviewPlan_FarLaneRoutesBackward(t *testing.T) {
	env := newRestatementEnv(t, 4)
	env.seedMotifPair("measure-becomes-target")

	payloads, _ := env.motifBridgeSession(EffortHigh)
	motifs := motifPayloads(payloads)

	require.Len(t, motifs, 1)
	require.Equal(t, LaneFar, motifs[0].Lane,
		"precondition: with no SIMILAR_TO edge the pair is a far-lane group")
	require.Equal(t, DiscoverBackward, motifs[0].Direction)
	require.Equal(t, "measure-becomes-target", motifs[0].Bridge.Token)
	require.Len(t, motifs[0].Bridge.Members, 2)
}

// The members' MOTIFS must survive into the payload the agent is served. The
// prompt's far-lane line names the motif, and the schema's carry-over
// instruction asks the agent to consider it — both read this field, and Phase 1
// shipped a version of exactly this seam that read back empty.
func TestReviewPlan_MotifPayloadCarriesMemberMotifs(t *testing.T) {
	env := newRestatementEnv(t, 4)
	env.seedMotifPair("measure-becomes-target")

	payloads, _ := env.motifBridgeSession(EffortHigh)
	motifs := motifPayloads(payloads)
	require.Len(t, motifs, 1)

	for _, m := range motifs[0].Bridge.Members {
		require.Contains(t, m.Motifs, "measure-becomes-target",
			"the member projection must carry the motif the group was formed on")
	}
}

// TestReviewPlan_NormalEmitsNoMotifItemsWithoutVerbatimMatches — MN5's contract
// seen from the review side: a corpus whose facts carry no motifs produces no
// motif work at normal effort, so the EffortNormal test passes vacuously.
func TestReviewPlan_NormalEmitsNoMotifItemsOnAMotifFreeCorpus(t *testing.T) {
	env := newRestatementEnv(t, 6)

	payloads, health := env.motifBridgeSession(EffortNormal)

	require.Empty(t, motifPayloads(payloads))
	require.NotContains(t, strings.Join(health, "\n"), "motif bridges:",
		"a corpus with nothing to say about motifs says nothing")
}

// TestReviewPlan_HealthReportsTheOperatingPoint — the descriptors are a
// fingerprint of the corpus, and the GATE package needs them to state what the
// gate DID rather than that it exists. Nothing branches on them.
func TestReviewPlan_HealthReportsTheOperatingPoint(t *testing.T) {
	env := newRestatementEnv(t, 4)
	env.seedMotifPair("measure-becomes-target")

	_, health := env.motifBridgeSession(EffortHigh)
	joined := strings.Join(health, "\n")

	require.Contains(t, joined, "motif bridges: 1 candidates, 0 near, 1 far")
	require.Contains(t, joined, "motif disjointness:")
	require.Contains(t, joined, "umbrella df >")
}

// TestReviewPlan_DiscoverItemsShareOneRankSpace — the motif lanes were added to
// the SAME forward-discover priority band as the entity/domain bridges, off one
// shared rank counter. That is deliberate (the band belongs to the review
// session's queue, not to a bridge kind), and it is also the regression risk
// the change introduced: two items landing on one priority, or on one cluster
// key, would make the queue order arbitrary.
//
// Asserted on the queue itself, which is the thing the engine reads.
func TestReviewPlan_DiscoverItemsShareOneRankSpace(t *testing.T) {
	env := newRestatementEnv(t, 4)
	env.seedMotifPair("measure-becomes-target")
	env.seedActivationVocabulary()
	env.rewriteWithMotifs("kb/gotchas/caching/staleread.md",
		"A cache read served state the writer had already replaced",
		"The reader observed a version the writer believed was gone.",
		[]string{"stale-read-after-write"}, []string{"Redis"})
	env.rewriteWithMotifs("kb/technology/ledgers/settlement.md",
		"Settlement confirmed against a balance that had already moved",
		"The confirmation was computed from a snapshot the ledger had superseded.",
		[]string{"stale-read-after-write"}, []string{"SWIFT"})

	res, err := NewReviewerWithOptions(env.ri, nil, EffortHigh, ScopeFilter{}).StartSession(context.Background())
	require.NoError(t, err)

	keys := map[string]bool{}
	priorities := map[float64]string{}
	discovers := 0
	for _, item := range env.workItems(res.SessionID) {
		if item.StepType != "discover" {
			continue
		}
		discovers++
		require.False(t, keys[item.ClusterKey], "duplicate cluster key %q", item.ClusterKey)
		keys[item.ClusterKey] = true
		prev, clash := priorities[item.Priority]
		require.False(t, clash, "%q and %q share priority %v", prev, item.ClusterKey, item.Priority)
		priorities[item.Priority] = item.ClusterKey
		require.LessOrEqual(t, item.Priority, float64(forwardDiscoverPriorityBase),
			"discover items sit below the grounded work")
		require.Greater(t, item.Priority, float64(reflectPriority),
			"and strictly above reflect — the band's whole purpose")
	}
	require.Equal(t, 2, discovers, "precondition: two motif groups, so the rank space is actually shared")
}
