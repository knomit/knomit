package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// reinforceEnv is a real store holding two seed facts, one unrelated fact the
// target already cites, and the target itself.
type reinforceEnv struct {
	*restatementEnv
	payload DiscoverWorkPayload
	repoID  string
}

const (
	seedOnePath   = "kb/gotchas/uitesting/agentclicks.md"
	seedTwoPath   = "kb/technology/benchmarks/terminalscores.md"
	existingRef   = "kb/conventions/verifiers/sandboxing.md"
	reinforcePath = "kb/principles/evaluation/verifierreach.md"
)

func newReinforceEnv(t *testing.T) *reinforceEnv {
	t.Helper()
	env := newRestatementEnv(t, 0)
	ctx := context.Background()

	write := func(path, title string, refs []string, sources int) {
		f := fact.NewFact(path)
		f.Title = title
		f.Body = "Body of " + title + "."
		f.Type = fact.Observation
		f.Confidence = 0.8
		f.Sources = sources
		f.Refs = refs
		content, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = env.svc.Facts().WriteFact(ctx, env.branch, path, content, "write "+path, "test")
		require.NoError(t, err)
	}

	write(seedOnePath, "An agent testing a UI will execute JavaScript instead of clicking", nil, 1)
	write(seedTwoPath, "Coding agents cheated a terminal benchmark to 94%", nil, 1)
	write(existingRef, "Verifiers run in a sandbox the agent cannot write to", nil, 1)

	target := fact.NewFact(reinforcePath)
	target.Title = "The verifier is inside the agent's action space"
	target.Body = "Every recorded form of eval gaming is an action taken ON the measurement apparatus."
	target.Type = fact.Synthesis
	target.Confidence = 0.75
	target.Sources = 1
	target.Domain = []string{"evaluation"}
	target.Entities = []string{"verifier gaming"}
	target.Motifs = []string{"measure-becomes-target"}
	target.Refs = []string{existingRef}
	content, err := fact.SerializeFact(target)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(ctx, env.branch, reinforcePath, content, "write target", "test")
	require.NoError(t, err)

	return &reinforceEnv{
		restatementEnv: env,
		repoID:         fact.ID12(env.ri.ID()),
		payload: DiscoverWorkPayload{
			Direction: DiscoverBackward,
			Lane:      LaneFar,
			Bridge: BridgeSeedSet{
				Token: "measure-becomes-target",
				Kind:  BridgeMotif,
				Members: []factForLLM{
					{File: seedOnePath}, {File: seedTwoPath},
				},
			},
		},
	}
}

func (e *reinforceEnv) apply(rs ...FactReinforcement) []string {
	e.t.Helper()
	written, err := applyReinforcements(context.Background(), e.svc.Facts(), e.svc.Search(),
		e.payload, rs, e.branch, e.repoID, func(ProgressEvent) {})
	require.NoError(e.t, err)
	return written
}

func (e *reinforceEnv) read(path string) fact.Fact {
	e.t.Helper()
	res, err := e.svc.Facts().ReadFact(context.Background(), e.branch, path, nil)
	require.NoError(e.t, err)
	f, err := fact.ParseFact(path, res.Content)
	require.NoError(e.t, err)
	return f
}

func goodReinforcement() FactReinforcement {
	return FactReinforcement{
		Path:   reinforcePath,
		Reason: "Both state that the verifier sits inside the agent's reachable action space.",
		Refs:   flexStrings{seedOnePath, seedTwoPath},
	}
}

// TestApplyReinforcements_ChangesNothingButRefsSourcesAndWeight — lesson 6:
// treat a write-path mistake AS DATA LOSS until proven otherwise, and guard
// everything the write must NOT change rather than the fields it does. The
// damage from the Phase-2 near-miss was invisible from the field being edited.
func TestApplyReinforcements_ChangesNothingButRefsSourcesAndWeight(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)

	written := env.apply(goodReinforcement())
	require.Equal(t, []string{reinforcePath}, written)

	after := env.read(reinforcePath)

	require.Equal(t, before.Title, after.Title)
	require.Equal(t, before.Body, after.Body)
	require.Equal(t, before.Type, after.Type)
	require.Equal(t, before.Kind, after.Kind)
	require.Equal(t, before.Domain, after.Domain)
	require.Equal(t, before.Entities, after.Entities)
	require.Equal(t, before.Motifs, after.Motifs)
	require.Equal(t, before.Confidence, after.Confidence)
	require.Equal(t, before.Origin, after.Origin)

	require.Equal(t, before.Sources+1, after.Sources)
	require.Greater(t, after.EvidenceWeight, before.EvidenceWeight)

	// APPEND, never replace: the fact's existing derivation path survives.
	require.Len(t, after.Refs, 3)
	for _, want := range []string{existingRef, seedOnePath, seedTwoPath} {
		require.Conditionf(t, func() bool {
			for _, r := range after.Refs {
				if r == want || contains(r, want) {
					return true
				}
			}
			return false
		}, "refs %v must include %s", after.Refs, want)
	}

	// Preconditions (lesson 5): the fields compared above must actually carry
	// values, or "unchanged" is a comparison of two empty strings.
	require.NotEmpty(t, before.Motifs)
	require.NotEmpty(t, before.Entities)
	require.NotEmpty(t, before.Refs)
}

// 0ee925f4: passing the SAME slice as refs and prior silently disables the
// gate. Proved by planting a ref that cannot resolve — the gate must refuse the
// whole write rather than let it through.
func TestApplyReinforcements_RefsGateIsLiveOnThisPath(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement()
	r.Refs = append(r.Refs, "kb/does/not/exist.md")
	require.Empty(t, env.apply(r))

	after := env.read(reinforcePath)
	require.Equal(t, before.Refs, after.Refs, "a refused write changes nothing")
	require.Equal(t, before.Sources, after.Sources)
}

// Default-NO on equivalence: an unreasoned claim is exactly the hallucinated
// one, so it is rejected rather than recorded with a blank justification.
func TestApplyReinforcements_RejectsAnUnreasonedEquivalence(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement()
	r.Reason = "   "
	require.Empty(t, env.apply(r))
	require.Equal(t, before.Sources, env.read(reinforcePath).Sources)
}

// The same engagement proof the proposal path demands: cite every seed.
func TestApplyReinforcements_RejectsWhenRefsDoNotCoverEverySeed(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement()
	r.Refs = flexStrings{seedOnePath}
	require.Empty(t, env.apply(r))
	require.Equal(t, before.Sources, env.read(reinforcePath).Sources)
}

// A fact cannot be an independent derivation of itself.
func TestApplyReinforcements_RejectsReinforcingASeed(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(seedOnePath)

	r := goodReinforcement()
	r.Path = seedOnePath
	require.Empty(t, env.apply(r))
	require.Equal(t, before.Sources, env.read(seedOnePath).Sources)
}

func TestApplyReinforcements_RejectsAnAbsentTarget(t *testing.T) {
	env := newReinforceEnv(t)
	r := goodReinforcement()
	r.Path = "kb/principles/evaluation/nothinghere.md"
	require.Empty(t, env.apply(r))
}

// Idempotency is the difference between "corroborated by two independent
// derivations" and "reviewed twice". A repeat must not inflate sources.
func TestApplyReinforcements_IsIdempotent(t *testing.T) {
	env := newReinforceEnv(t)

	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()))
	once := env.read(reinforcePath)
	require.Equal(t, 2, once.Sources)

	require.Empty(t, env.apply(goodReinforcement()),
		"every seed is already a derivation path — there is nothing new to record")
	twice := env.read(reinforcePath)
	require.Equal(t, once.Sources, twice.Sources)
	require.Equal(t, once.Refs, twice.Refs)
	require.Equal(t, once.EvidenceWeight, twice.EvidenceWeight)
}

// A second, DIFFERENT bridge reinforcing the same fact is not a repeat: it is a
// third derivation path, and it must count.
func TestApplyReinforcements_ADifferentDerivationStillCounts(t *testing.T) {
	env := newReinforceEnv(t)
	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()))
	first := env.read(reinforcePath)

	// A new seed, and a bridge that cites it.
	const seedThree = "kb/incidents/graders/scriptedit.md"
	f := fact.NewFact(seedThree)
	f.Title = "An agent edited the grading script instead of passing the test"
	f.Body = "The run was scored by a script the agent could write to."
	f.Type = fact.Observation
	f.Confidence = 0.8
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, seedThree, content, "write", "test")
	require.NoError(t, err)

	env.payload.Bridge.Members = append(env.payload.Bridge.Members, factForLLM{File: seedThree})
	r := goodReinforcement()
	r.Refs = flexStrings{seedOnePath, seedTwoPath, seedThree}

	require.Equal(t, []string{reinforcePath}, env.apply(r))
	after := env.read(reinforcePath)
	require.Equal(t, first.Sources+1, after.Sources)
	require.Len(t, after.Refs, 4)
}

// contains reports whether a canonicalised ref names the given local path.
// Refs are stored as kb://<repo-id>/<path>, so a bare-path comparison alone
// would miss them.
func contains(ref, path string) bool {
	return len(ref) >= len(path) && ref[len(ref)-len(path):] == path
}
