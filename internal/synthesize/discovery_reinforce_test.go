package synthesize

import (
	"context"
	"slices"
	"strings"
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
	// Origin EXPLICIT. A synthesis fact with the line elided does not
	// round-trip — ParseFact defaults it to distilled and SerializeFact then
	// writes it — so under the write contract such a target is SKIPPED. Every
	// pipeline-written synthesis fact carries it; the fixture matches.
	target.Origin = fact.Distilled
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
	return applyReinforcements(context.Background(), e.svc.Facts(), e.svc.Search(),
		e.payload, rs, e.branch, e.repoID, func(ProgressEvent) {})
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
// gate. It must still be live on this path — and after H2's fix that can only
// be shown by INJECTION, which is worth stating plainly rather than dressing a
// weaker test as a strong one.
//
// Two reasons no ordinary input reaches it. Extras are discarded before the
// gate sees them, so a bogus ref the model names never gets there. And a
// retracted seed still RESOLVES: refs are historical by design (a ref is
// `fact` rather than `broken` when the target has any version visible at the
// source's anchor), so retracting a seed does not break the edge that cites it.
//
// So the gate is defence in depth against a future path that supplies refs some
// other way, and this drives it with a bridge member that never existed. Keep
// it for the same reason the origin fence is kept: it is what stops the next
// caller from writing a broken edge, and a check nothing exercises is a check
// nobody notices going missing.
func TestApplyReinforcements_RefsGateIsLiveOnThisPath(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)

	const ghost = "kb/gotchas/never/existed.md"
	env.payload.Bridge.Members = append(env.payload.Bridge.Members, factForLLM{File: ghost})

	r := goodReinforcement()
	r.Refs = flexStrings{seedOnePath, seedTwoPath, ghost}

	require.Empty(t, env.apply(r),
		"a seed that resolves to nothing must refuse the write, not be written as a broken edge")

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

// The same derivation-path floor the proposal path demands: at least two seeds.
//
// Retargeted by #151 (was ..._RejectsWhenRefsDoNotCoverEverySeed). This env's
// bridge has two members, so citing one is both "not every seed" and "below the
// floor" — the two rules coincide here and this test cannot tell them apart.
// The three-seed cases that CAN are in discovery_citation_test.go.
func TestApplyReinforcements_RejectsFewerThanTwoCitedSeeds(t *testing.T) {
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

// storedBytes reads the fact exactly as the corpus holds it. The guard below
// compares THESE, not parsed structs — see the test's own comment for why.
func (e *reinforceEnv) storedBytes(path string) string {
	e.t.Helper()
	res, err := e.svc.Facts().ReadFact(context.Background(), e.branch, path, nil)
	require.NoError(e.t, err)
	return res.Content
}

// changedLines returns the lines that differ between two versions of a fact.
func changedLines(before, after string) []string {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	seen := map[string]struct{}{}
	for _, l := range b {
		seen[l] = struct{}{}
	}
	var out []string
	for _, l := range a {
		if _, ok := seen[l]; !ok {
			out = append(out, l)
		}
	}
	for _, l := range b {
		if !slices.Contains(a, l) {
			out = append(out, l)
		}
	}
	return out
}

// TestApplyReinforcements_ChangesOnlyPermittedStoredLines — the guard at the
// boundary the CORPUS reads (lesson 8, review H1).
//
// Its predecessor compared parsed structs, and passed while the write was
// deleting motifs: the lenient parser dropped the malformed motif on BOTH sides
// of the comparison, so it compared two already-truncated lists. That history
// is why this assertion is on bytes and why the struct comparison is kept only
// as a secondary check.
func TestApplyReinforcements_ChangesOnlyPermittedStoredLines(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.storedBytes(reinforcePath)

	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()))
	after := env.storedBytes(reinforcePath)

	require.NotEqual(t, before, after, "precondition: the write must have done something")
	for _, line := range changedLines(before, after) {
		field := strings.SplitN(strings.TrimSpace(line), ":", 2)[0]
		require.Containsf(t, []string{"refs", "sources", "evidence_weight", "-"}, field,
			"a reinforcement changed the stored line %q — only refs, sources and "+
				"evidence_weight may differ", line)
	}
}

// H1: a target whose motifs do not survive the lenient parser is SKIPPED, not
// silently normalised. A fact carrying legacy data is not this path's to clean.
func TestApplyReinforcements_SkipsALossyTarget(t *testing.T) {
	env := newReinforceEnv(t)

	// A motif that today's rules reject, as a pre-Phase-1 fact or one synced
	// from a remote written by another version would carry it. Written as raw
	// bytes because SerializeFact would refuse it — which is the point.
	lossy := strings.Replace(env.storedBytes(reinforcePath),
		"motifs: [measure-becomes-target]", "motifs: [measure-becomes-target, legacy]", 1)
	require.Contains(t, lossy, ", legacy]", "precondition: the fixture planted the motif")
	_, err := env.svc.Facts().WriteFact(context.Background(), env.branch, reinforcePath,
		lossy, "plant legacy motif", "test")
	require.NoError(t, err)

	require.Empty(t, env.apply(goodReinforcement()), "a lossy target is skipped")
	require.Equal(t, lossy, env.storedBytes(reinforcePath),
		"and its bytes are untouched — including the motif the parser would have dropped")

	// WHICH check refused it. The meaning-preservation check would also catch
	// this (the motifs line changes), so a test that only observes the skip
	// cannot tell the two apart — sabotaging the tripwire leaves it green. The
	// tripwire's value is the REASON it gives, so the reason is what is
	// asserted: "rewritten outside the permitted lines" would send a reader
	// hunting a formatting problem instead of a deleted motif.
	var warnings []string
	applyReinforcements(context.Background(), env.svc.Facts(), env.svc.Search(),
		env.payload, []FactReinforcement{goodReinforcement()}, env.branch, env.repoID,
		func(e ProgressEvent) {
			if e.Phase == "warn" {
				warnings = append(warnings, e.Message)
			}
		})
	require.NotEmpty(t, warnings)
	require.Contains(t, strings.Join(warnings, "\n"), "cannot round-trip",
		"the skip must name the motifs it protected, not just report a rewrite")
	require.Contains(t, strings.Join(warnings, "\n"), "legacy")
}

// L9, as ruled in phase3-rulings-5: an origin line materialising to exactly the
// parse default is a SEMANTIC NO-OP and is permitted — the measured 144 facts.
// The fact reinforces normally, and the materialised line says what ParseFact
// already said the fact meant.
func TestApplyReinforcements_PermitsAnOriginMaterialisingToTheParseDefault(t *testing.T) {
	env := newReinforceEnv(t)

	elided := env.storedBytes(reinforcePath)
	require.Contains(t, elided, "origin: distilled", "precondition: the fixture has one to remove")
	elided = strings.Replace(elided, "origin: distilled\n", "", 1)
	_, err := env.svc.Facts().WriteFact(context.Background(), env.branch, reinforcePath,
		elided, "elide origin", "test")
	require.NoError(t, err)
	require.Equal(t, fact.Distilled, env.read(reinforcePath).Origin,
		"precondition: the parse default for this fact IS what would materialise")

	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()),
		"a semantic no-op does not cost the corroboration")
	require.Contains(t, env.storedBytes(reinforcePath), "origin: distilled")
}

// The fence on that exception, driven through the comparison helper DIRECTLY.
//
// It has to be: on the reinforcement path nothing mutates Origin between parse
// and serialize, so no ordinary input can make the two parsed origins differ,
// and a test that reinforces a normal fact and observes success would not touch
// this clause at all — the Phase-1 MN4 shape, where a comment satisfied a check
// that never ran (reviewer note, rulings-5).
//
// The case it fences is measured and real: 66 core facts whose stored
// type/origin pairing is illegal, which ParseFact coerces to `authored`. A
// rewrite there would silently reattribute someone's provenance.
func TestRewriteIsMeaningPreserving_FencesAnOriginThatWouldCHANGE(t *testing.T) {
	const stored = "---\ntype: synthesis\nconfidence: 0.75\nsources: 1\nrefs: []\n---\n# T\n\nBody.\n"
	const rewritten = "---\ntype: synthesis\nconfidence: 0.75\nsources: 2\norigin: authored\nrefs: []\n---\n# T\n\nBody.\n"

	// Same parsed origin on both sides: the measured no-op, permitted.
	why, ok := rewriteIsMeaningPreserving(stored, rewritten, fact.Authored, fact.Authored)
	require.True(t, ok, "a materialised origin equal to the parse default is a no-op: %s", why)

	// DIFFERING parsed origins — the mutation injected, since the live path
	// cannot produce it. This is the clause the fence exists for.
	why, ok = rewriteIsMeaningPreserving(stored, rewritten, fact.Distilled, fact.Authored)
	require.False(t, ok, "an origin materialising to something other than the parse default must skip")
	require.Contains(t, why, "not the parsed default")
}

// And the neighbouring clauses of the same helper, each with a real diff.
func TestRewriteIsMeaningPreserving_RefusesEveryOtherLineChange(t *testing.T) {
	const stored = "---\ntype: observation\nconfidence: 0.8\nsources: 1\nmotifs: [shape-of-thing]\nrefs: []\n---\n# T\n\nBody.\n"

	for name, rewritten := range map[string]string{
		"a dropped motif":      "---\ntype: observation\nconfidence: 0.8\nsources: 2\nrefs: []\n---\n# T\n\nBody.\n",
		"a changed motif":      "---\ntype: observation\nconfidence: 0.8\nsources: 2\nmotifs: [other-shape]\nrefs: []\n---\n# T\n\nBody.\n",
		"a changed confidence": "---\ntype: observation\nconfidence: 0.9\nsources: 2\nmotifs: [shape-of-thing]\nrefs: []\n---\n# T\n\nBody.\n",
		"a changed body":       "---\ntype: observation\nconfidence: 0.8\nsources: 2\nmotifs: [shape-of-thing]\nrefs: []\n---\n# T\n\nDifferent body.\n",
	} {
		_, ok := rewriteIsMeaningPreserving(stored, rewritten, fact.Authored, fact.Authored)
		require.Falsef(t, ok, "%s must skip", name)
	}

	// The permitted lines, and the rendering difference the ruling names
	// explicitly: quote style on the refs line is not a value change.
	for name, rewritten := range map[string]string{
		"only the permitted lines": "---\ntype: observation\nconfidence: 0.8\nsources: 2\nmotifs: [shape-of-thing]\nrefs: [kb/a.md]\nevidence_weight: 0.5\n---\n# T\n\nBody.\n",
		"refs quote style":         "---\ntype: observation\nconfidence: 0.8\nsources: 1\nmotifs: [shape-of-thing]\nrefs: ['kb/a.md']\n---\n# T\n\nBody.\n",
	} {
		why, ok := rewriteIsMeaningPreserving(stored, rewritten, fact.Authored, fact.Authored)
		require.Truef(t, ok, "%s must be permitted: %s", name, why)
	}
}

// H2: refs the model names beyond the bridge's seeds are DISCARDED, and the
// valid reinforcement still lands. Surplus citation does not kill it.
func TestApplyReinforcements_DiscardsRefsBeyondTheSeeds(t *testing.T) {
	env := newReinforceEnv(t)

	// A live fact with no relationship to this bridge — the shape an agent
	// produces when it lists what it read while deciding.
	const unrelated = "kb/gotchas/unrelated/somethingelse.md"
	f := fact.NewFact(unrelated)
	f.Title = "An unrelated fact"
	f.Body = "Nothing to do with the bridge."
	f.Type = fact.Observation
	f.Confidence = 0.8
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, unrelated, content, "write", "test")
	require.NoError(t, err)

	r := goodReinforcement()
	r.Refs = flexStrings{seedOnePath, seedTwoPath, unrelated}

	require.Equal(t, []string{reinforcePath}, env.apply(r), "the reinforcement still lands")

	after := env.read(reinforcePath)
	require.Len(t, after.Refs, 3, "existing + two seeds, and nothing else")
	for _, ref := range after.Refs {
		require.NotContains(t, ref, "somethingelse",
			"a ref the model named beyond the seeds must never become a derivation edge")
	}
}

// L8: the strings already on the fact are written back exactly as they were.
func TestApplyReinforcements_DoesNotRewriteExistingRefs(t *testing.T) {
	env := newReinforceEnv(t)
	before := env.read(reinforcePath)
	require.Equal(t, []string{existingRef}, before.Refs,
		"precondition: the existing ref is stored in bare-path form")

	require.Equal(t, []string{reinforcePath}, env.apply(goodReinforcement()))

	after := env.read(reinforcePath)
	require.Contains(t, after.Refs, existingRef,
		"the existing ref keeps its exact string — canonicalising it would be a "+
			"mutation of authored data outside 'append the seeds'")
}

// warnsOf runs a reinforcement and returns what was written alongside the
// warnings raised, so a test can assert WHICH gate refused rather than only
// that something did.
func warnsOf(env *reinforceEnv, rs ...FactReinforcement) ([]string, []string) {
	var warns []string
	written := applyReinforcements(context.Background(), env.svc.Facts(), env.svc.Search(),
		env.payload, rs, env.branch, env.repoID, func(e ProgressEvent) {
			if e.Phase == "warn" {
				warns = append(warns, e.Message)
			}
		})
	return written, warns
}

// TestApplyReinforcements_SkipsAnIllegalTypeOriginPairing binds the fidelity
// check to the WRITE PATH (review M5).
//
// rewriteIsMeaningPreserving had thorough tests of its own, and none of them
// reached it through applyReinforcements: deleting the call left the entire
// repo suite green. The two write-path tests that should have caught it each
// shadowed the other — the lossy-motif case is refused by the tripwire before
// the fidelity check runs, and the byte-guard fixture round-trips cleanly, so
// nothing arrived at the check by the front door. This test is that binding.
//
// The population is measured, not hypothetical: 66 live facts on `core` are
// stored with a type/origin pairing ValidateForType rejects (33 `observation` +
// `discovered`, 33 `observation` + `distilled`). ParseFact coerces them to
// `authored` and SerializeFact then ELIDES the now-default line, so a write
// that does not refuse moves 33 discovery-engine facts to human-authored
// provenance — the belief-level discipline designer-intent item 3 forbids.
func TestApplyReinforcements_SkipsAnIllegalTypeOriginPairing(t *testing.T) {
	env := newReinforceEnv(t)

	// The exact illegal pairing, planted as RAW BYTES: SerializeFact would
	// refuse to produce it, which is why such facts can only arrive from an
	// older version or another writer.
	raw := "---\ntype: observation\ndomain: [evaluation]\nconfidence: 0.75\nsources: 1\n" +
		"origin: discovered\nentities: [verifier gaming]\nrefs: [" + existingRef + "]\n---\n" +
		"# The verifier is inside the agent's action space\n\nBody text.\n"
	_, err := env.svc.Facts().WriteFact(context.Background(), env.branch, reinforcePath, raw, "plant", "test")
	require.NoError(t, err)

	// PRECONDITIONS, asserted rather than logged. If ParseFact ever stops
	// coercing this pairing, or SerializeFact stops eliding the default line,
	// the assertions below would go on passing while testing nothing.
	parsed := env.read(reinforcePath)
	require.Equal(t, fact.Authored, parsed.Origin,
		"precondition: the stored `discovered` is coerced on read — that coercion is the danger")
	round, err := fact.SerializeFact(parsed)
	require.NoError(t, err)
	require.NotContains(t, round, "origin:",
		"precondition: the round trip DELETES the line, which is what a write would persist")

	written, warns := warnsOf(env, goodReinforcement())

	require.Empty(t, written, "an illegal-pairing target must be SKIPPED, not reattributed")
	require.Equal(t, raw, env.storedBytes(reinforcePath),
		"and its bytes untouched, byte for byte — `origin: discovered` survives. "+
			"Compared against the planted BYTES, not a parsed struct: ParseFact coerces "+
			"the origin on both sides, so a struct comparison would read authored == "+
			"authored and pass while the file was rewritten (the H1 mistake)")
	require.Contains(t, strings.Join(warns, "\n"), "origin",
		"the refusal must name the origin, or the assertion cannot tell the fidelity "+
			"check from any other refusal — which is the shadowing that produced M5")
}
