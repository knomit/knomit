package synthesize

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// Phase-3 multi-session dynamics — the standing requirement.
//
// It exists because the Phase-0 review found every component unit-correct and
// every blocking defect in the CROSS-SESSION behaviour. A gate that computes
// the right operating point once, an alias table that resolves once, and a
// bridge that enumerates once can all still be wrong the second time a session
// runs over the same corpus.

// motifDynamicsEnv is a corpus big enough for the gate's RATIOS to mean
// something.
//
// The size is load-bearing, not padding. The umbrella rule is "df > 20% of the
// corpus", so on a six-fact corpus the umbrella cut is 1 — and since a SHARED
// label has df >= 2 by definition, every shared label reads as an umbrella and
// the disjointness gate cannot block anything at all. At forty facts the cut is
// 8 and a two-carrier entity is what it actually is: rare, and specific.
//
// (The degenerate case is real but bounded: a corpus that small produces almost
// no motif candidates, and Phase 4's activation gate keeps the axis off such
// corpora entirely.)
func motifDynamicsEnv(t *testing.T) *restatementEnv {
	t.Helper()
	return newRestatementEnv(t, 40)
}

// rewriteWithMotifs replaces a fact in place, carrying explicit entities so a
// test can move it in or out of subject-disjointness.
func (e *restatementEnv) rewriteWithMotifs(path, title, body string, motifs, entities []string) {
	e.t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = fact.Observation
	f.Confidence = 0.7
	f.Sources = 1
	f.Motifs = motifs
	f.Entities = entities
	content, err := fact.SerializeFact(f)
	require.NoError(e.t, err)
	_, err = e.svc.Facts().WriteFact(context.Background(), e.branch, path, content, "write "+path, "test")
	require.NoError(e.t, err)
}

// TestMotifBridging_AcrossSessions runs five sessions over one corpus and
// asserts things only their SEQUENCE can establish.
func TestMotifBridging_AcrossSessions(t *testing.T) {
	env := motifDynamicsEnv(t)
	const (
		uiPath    = "kb/gotchas/uitesting/agentclicks.md"
		benchPath = "kb/technology/benchmarks/terminalscores.md"
	)

	// S1 — no motifs anywhere. The axis must do nothing, and say nothing.
	s1, h1 := env.motifBridgeSession(EffortHigh)
	require.Empty(t, motifPayloads(s1))
	require.NotContains(t, strings.Join(h1, "\n"), "motif bridges:")

	// S2 — the motifs arrive on two subject-disjoint facts. The bridge forms,
	// on a corpus whose alias table this session had to build for itself.
	env.rewriteWithMotifs(uiPath,
		"An agent testing a UI will execute JavaScript instead of clicking",
		"Driving app state directly bypasses the path the verifier believes it is checking.",
		[]string{"measure-becomes-target"}, []string{"Cognition"})
	env.rewriteWithMotifs(benchPath,
		"Coding agents reached 94% on a terminal benchmark by cheating it",
		"The agents scored by exploiting the harness rather than by solving the tasks.",
		[]string{"measure-becomes-target"}, []string{"Terminal Bench"})

	s2, h2 := env.motifBridgeSession(EffortHigh)
	motifs2 := motifPayloads(s2)
	require.Len(t, motifs2, 1, "the pair bridges once its motifs exist")
	require.Equal(t, LaneFar, motifs2[0].Lane)
	require.Contains(t, strings.Join(h2, "\n"), "motif bridges: 1 candidates")

	// S3 — nothing changed. The bridge is still THERE: enumeration is a
	// function of the live corpus, not of what this session happens to have
	// touched, so a standing bridge does not blink out because a session was
	// quiet.
	s3, _ := env.motifBridgeSession(EffortHigh)
	require.Len(t, motifPayloads(s3), 1,
		"a bridge is a property of the corpus, not of the session's dirty set")

	// S4 — one member is rewritten so the two now share a RARE entity. They are
	// one subject, and the gate must RECOMPUTE rather than remember: the bridge
	// disappears without anything retracting it.
	env.rewriteWithMotifs(benchPath,
		"Coding agents reached 94% on a terminal benchmark by cheating it",
		"The agents scored by exploiting the harness rather than by solving the tasks.",
		[]string{"measure-becomes-target"}, []string{"Cognition"})

	s4, _ := env.motifBridgeSession(EffortHigh)
	require.Empty(t, motifPayloads(s4),
		"the pair now shares a rare entity — level-triggered, so the gate re-decides")

	// S5 — and back. The same edit reversed restores the bridge, which is what
	// makes S4 the gate working rather than the axis breaking.
	env.rewriteWithMotifs(benchPath,
		"Coding agents reached 94% on a terminal benchmark by cheating it",
		"The agents scored by exploiting the harness rather than by solving the tasks.",
		[]string{"measure-becomes-target"}, []string{"Terminal Bench"})

	s5, _ := env.motifBridgeSession(EffortHigh)
	require.Len(t, motifPayloads(s5), 1)
}

// TestMotifBridging_EffortIsRecomputedPerSession — the same corpus, two
// sessions, two efforts. Nothing about the axis is persisted, so lowering
// effort must close the far lane on the very next session.
func TestMotifBridging_EffortIsRecomputedPerSession(t *testing.T) {
	env := motifDynamicsEnv(t)
	env.rewriteWithMotifs("kb/gotchas/uitesting/agentclicks.md",
		"An agent testing a UI will execute JavaScript instead of clicking",
		"Driving app state directly bypasses the verifier's path.",
		[]string{"measure-becomes-target"}, []string{"Cognition"})
	env.rewriteWithMotifs("kb/technology/benchmarks/terminalscores.md",
		"Coding agents reached 94% on a terminal benchmark by cheating it",
		"The agents exploited the harness rather than solving the tasks.",
		[]string{"measure-becomes-target"}, []string{"Terminal Bench"})

	high, _ := env.motifBridgeSession(EffortHigh)
	require.Len(t, motifPayloads(high), 1, "the far lane is open at high")

	medium, _ := env.motifBridgeSession(EffortMedium)
	require.Empty(t, motifPayloads(medium),
		"medium is near lane only, and this group is far — no state carries the item over")
}

// TestMotifBridging_FromNothing — lesson 7. Hand-seeded fixtures structurally
// cannot detect a bootstrap deadlock: Phase 2 shipped an entire vocabulary
// lifecycle unreachable, with ~60 tests green, because every one of them seeded
// the alias table by hand.
//
// This starts from NOTHING derived — facts with motifs, no alias rows, no
// definitions, no title vectors — and requires the pipeline to build its own
// way to a motif bridge through real sessions.
func TestMotifBridging_FromNothing(t *testing.T) {
	env := newRestatementEnv(t, 0)

	// One regularity, on two subject-disjoint facts. The spelling is the same
	// on both deliberately: collapsing two SPELLINGS needs the alias judge,
	// which needs an agent to answer its work item, and this test is about the
	// mechanical derived state a session must build unaided — the alias table,
	// the canonical id, the df — not about the judged half.
	env.rewriteWithMotifs("kb/gotchas/caching/staleread.md",
		"A cache served state the writer had already replaced",
		"The reader observed a version the writer believed was gone.",
		[]string{"stale-read-after-write"}, []string{"Redis"})
	env.rewriteWithMotifs("kb/technology/ledgers/settlement.md",
		"Settlement confirmed against a balance that had already moved",
		"The confirmation was computed from a snapshot the ledger had superseded.",
		[]string{"stale-read-after-write"}, []string{"SWIFT"})

	var found bool
	for i := 0; i < 4 && !found; i++ {
		payloads, _ := env.motifBridgeSession(EffortHigh)
		found = len(motifPayloads(payloads)) > 0
	}
	require.True(t, found,
		"a session must maintain the derived state its own bridging depends on")
}

// TestMotifBridging_ReinforcementSurvivesLaterSessions — the write REINFORCE
// makes must still be there when the next session reads the corpus, and must
// not be re-applied by it.
func TestMotifBridging_ReinforcementSurvivesLaterSessions(t *testing.T) {
	env := newReinforceEnv(t)

	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()))
	afterWrite := env.read(reinforcePath)
	require.Equal(t, 2, afterWrite.Sources)

	// A whole session runs over the corpus in between.
	_, _ = env.motifBridgeSession(EffortHigh)

	afterSession := env.read(reinforcePath)
	require.Equal(t, afterWrite.Sources, afterSession.Sources)
	require.Equal(t, afterWrite.Refs, afterSession.Refs)
	require.Equal(t, afterWrite.EvidenceWeight, afterSession.EvidenceWeight)

	// And a second reinforcement attempt after that session is still a no-op.
	require.Empty(t, env.apply(goodReinforcement()))
	require.Equal(t, afterWrite.Sources, env.read(reinforcePath).Sources)
}
