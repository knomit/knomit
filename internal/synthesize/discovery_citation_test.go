package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// citationRepoID is a well-formed 12-hex repo id. Most of this package's
// discovery fixtures pass bareRefFixture ("") instead, which works only because
// they spell every ref as a bare path — see
// TestSeedSubset_EmptyRepoIDCannotSeeACanonicalSeed for why that is not a
// harmless simplification here.
const citationRepoID = "0123456789ab"

func seedSet(paths ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		out[p] = struct{}{}
	}
	return out
}

// ── the relaxation itself ──

// TestSeedSubset_ThreeSeedBridgeAcceptsTwo is #151 in one assertion, and the
// only test in the package that can show the relaxation at all: a bridge
// offering THREE seeds, a derived fact citing two.
//
// Under the all-seeds rule this was REJECTED, and that rejection is what the
// issue measured — s207 lost a valid independent derivation because four of
// five seeds supported the target and the fifth did not, with no subset
// mechanism to record it at its true strength.
func TestSeedSubset_ThreeSeedBridgeAcceptsTwo(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")

	require.True(t, refsCiteSeedSubset([]string{"kb/a.md", "kb/b.md"}, seeds, citationRepoID),
		"two of three offered seeds IS a derivation path — this is the whole of #151")
	require.True(t, refsCiteSeedSubset([]string{"kb/a.md", "kb/b.md", "kb/c.md"}, seeds, citationRepoID),
		"citing all three is still fine; the rule was relaxed, not inverted")
}

// TestSeedSubset_OneCitedSeedIsNotADerivation pins the floor. One seed is not a
// weaker bridge — it is a different claim shape, an observation about a single
// fact, which the bridge machinery is not the way to record.
func TestSeedSubset_OneCitedSeedIsNotADerivation(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")

	require.False(t, refsCiteSeedSubset([]string{"kb/a.md"}, seeds, citationRepoID))
	require.False(t, refsCiteSeedSubset(nil, seeds, citationRepoID),
		"an empty refs array means the inputs were not engaged with")
}

// TestSeedSubset_TwoSeedBridgeStillNeedsBoth states the consequence a reader is
// most likely to get wrong, and states it deliberately rather than leaving it
// to be discovered: on a TWO-seed bridge the new rule and the old one are
// IDENTICAL. #151 relaxes nothing there.
//
// This is not a gap. A bridge asserts a claim follows from the conjunction of
// facts clustering kept apart; with two seeds, both are the conjunction. The
// relaxation exists for the three-and-more case, which is where it was
// measured. If this test ever goes green on a single citation, the floor has
// been lowered to one and the bridge contract has changed meaning.
func TestSeedSubset_TwoSeedBridgeStillNeedsBoth(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md")

	require.False(t, refsCiteSeedSubset([]string{"kb/a.md"}, seeds, citationRepoID),
		"on a two-seed bridge, citing one is below the floor")
	require.True(t, refsCiteSeedSubset([]string{"kb/a.md", "kb/b.md"}, seeds, citationRepoID))
}

// ── the stored-canonical spelling ──

// TestSeedSubset_CanonicalSpellingCountsAsTheSeed is the load-bearing one.
//
// Refs are STORED CANONICAL as kb://<own-id12>/<path>. The predicate this
// replaced compared raw strings, so a seed cited in the spelling the corpus
// itself writes matched nothing. Under the all-seeds rule that failed loudly;
// under the subset rule it fails twice — the correctly-spelled seed does not
// COUNT toward the floor, and is then discarded as an extra, so relaxing the
// gate without normalising would have thrown away the very answers #151 exists
// to keep.
func TestSeedSubset_CanonicalSpellingCountsAsTheSeed(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")
	refs := []string{
		fact.QualifyKBPath(citationRepoID, "kb/a.md"),
		"kb/b.md",
	}
	// Fixture check: the ref really is in the stored form. A bare path here
	// would make this test pass with normalisation removed.
	require.Contains(t, refs[0], "kb://",
		"fixture must use the stored spelling or it does not exercise normalisation")

	require.True(t, refsCiteSeedSubset(refs, seeds, citationRepoID),
		"a canonically-spelled seed IS the seed and must count toward the floor")

	cited, extras, distinct := splitSeedRefs(refs, seeds, citationRepoID)
	require.Equal(t, 2, distinct)
	require.Len(t, cited, 2)
	require.Empty(t, extras, "a canonically-spelled seed must not be discarded as an extra")
}

// TestSeedSubset_EmptyRepoIDCannotSeeACanonicalSeed asserts a FAILURE MODE, not
// a feature — the same shape as #125's EmptyRepoIDExcludesNothing.
//
// With an empty local repo id every kb:// ref classifies as FOREIGN, so a
// correctly-cited seed is invisible: it does not count toward the floor and it
// is discarded as an extra. The two arms below are the same input and differ
// only in the repo id, so what they measure is the id and nothing else.
//
// If this ever flips, normalisation has stopped depending on the repo id and
// the hazard is gone — DELETE this test rather than weakening its assertion.
func TestSeedSubset_EmptyRepoIDCannotSeeACanonicalSeed(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")
	refs := []string{
		fact.QualifyKBPath(citationRepoID, "kb/a.md"),
		fact.QualifyKBPath(citationRepoID, "kb/b.md"),
	}

	require.True(t, refsCiteSeedSubset(refs, seeds, citationRepoID),
		"with the real repo id both seeds are recognised")

	require.False(t, refsCiteSeedSubset(refs, seeds, ""),
		"with an empty repo id the SAME two seeds are invisible — this is the trap being pinned")
	_, extras, distinct := splitSeedRefs(refs, seeds, "")
	require.Zero(t, distinct)
	require.Len(t, extras, 2, "and they are then thrown away as refs outside the bridge")
}

// TestSeedSubset_OneSeedTwoSpellingsCountsOnce stops a fact buying itself a
// second term by naming one seed twice. Distinctness is what makes the floor a
// claim about the ARGUMENT rather than about the length of the refs array.
func TestSeedSubset_OneSeedTwoSpellingsCountsOnce(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")
	refs := []string{"kb/a.md", fact.QualifyKBPath(citationRepoID, "kb/a.md")}

	_, _, distinct := splitSeedRefs(refs, seeds, citationRepoID)
	require.Equal(t, 1, distinct, "one seed, spelled two ways, is one term")
	require.False(t, refsCiteSeedSubset(refs, seeds, citationRepoID))
}

// TestSeedSubset_NonSeedRefsAreExtras covers the "nothing inventable" half:
// only offered seeds count, and everything else is the caller's to discard.
func TestSeedSubset_NonSeedRefsAreExtras(t *testing.T) {
	seeds := seedSet("kb/a.md", "kb/b.md", "kb/c.md")
	refs := []string{"kb/a.md", "kb/b.md", "kb/invented.md", "https://example.com/paper"}

	cited, extras, distinct := splitSeedRefs(refs, seeds, citationRepoID)
	require.Equal(t, 2, distinct, "an invented ref does not help reach the floor")
	require.ElementsMatch(t, []string{"kb/a.md", "kb/b.md"}, cited)
	require.ElementsMatch(t, []string{"kb/invented.md", "https://example.com/paper"}, extras,
		"an external URL is an extra too — the discover prompt asks for a derivation path, not a bibliography")
}

// ── the proposal path, end to end ──

// threeSeedProposalEnv is a real store with three seeds, for the cases the
// package's two-member fixtures structurally cannot express.
func threeSeedProposalEnv(t *testing.T) (*store.Service, string, DiscoverWorkPayload) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md"} {
		seedSimpleFact(t, svc, branch, p)
	}
	return svc, branch, DiscoverWorkPayload{
		Direction: DiscoverForward,
		Bridge: BridgeSeedSet{
			Token: "x", Kind: BridgeEntity,
			Members: []factForLLM{
				{File: "kb/a.md", Title: "A"},
				{File: "kb/b.md", Title: "B"},
				{File: "kb/c.md", Title: "C"},
			},
		},
	}
}

// TestApplyDiscoveredProposals_AcceptsTwoOfThreeSeeds is the behavioural half
// of the relaxation on the proposal path — the derivation that used to be
// discarded whole is now written.
func TestApplyDiscoveredProposals_AcceptsTwoOfThreeSeeds(t *testing.T) {
	svc, branch, payload := threeSeedProposalEnv(t)

	props := []DiscoveredFact{{
		Path: "kb/kept.md", Title: "kept", Body: "kept", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9,
		Refs: []string{"kb/a.md", "kb/b.md"}, // kb/c.md genuinely does not fit
	}}

	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(),
		nil, payload, props, DiscoveryGates{}, branch, bareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.Len(t, written, 1, "a two-of-three derivation must be recorded, not discarded")

	f := readFactForTest(t, svc, branch, written[0])
	require.Len(t, f.Refs, 2, "and it records only the seeds it actually rests on")
	require.NotContains(t, strings.Join(f.Refs, " "), "kb/c.md",
		"the seed that did not fit must not be forced into the derivation path")
}

// TestApplyDiscoveredProposals_DiscardsRefsOutsideTheBridge covers the
// offered-only half, which BEFORE #151 this path did not enforce at all: refs
// went through to the written fact unfiltered, so an invented citation became a
// permanent DERIVED_FROM edge on evidence nobody checked.
//
// Discard, not reject: the proposal survives with the invention removed. The
// pair of assertions is the point — that it was written AND that the invention
// is gone. Asserting only the first would pass on the pre-#151 behaviour.
func TestApplyDiscoveredProposals_DiscardsRefsOutsideTheBridge(t *testing.T) {
	svc, branch, payload := threeSeedProposalEnv(t)
	seedSimpleFact(t, svc, branch, "kb/elsewhere.md")

	props := []DiscoveredFact{{
		Path: "kb/kept.md", Title: "kept", Body: "kept", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9,
		Refs: []string{"kb/a.md", "kb/b.md", "kb/elsewhere.md"},
	}}

	var warned []string
	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(),
		nil, payload, props, DiscoveryGates{}, branch, bareRefFixture, "kb",
		func(e ProgressEvent) {
			if e.Phase == "warn" {
				warned = append(warned, e.Message)
			}
		})
	require.NoError(t, err)
	require.Len(t, written, 1, "surplus citation must not kill an otherwise valid proposal")

	f := readFactForTest(t, svc, branch, written[0])
	require.NotContains(t, strings.Join(f.Refs, " "), "kb/elsewhere.md",
		"a ref the bridge never offered must not become a derivation path")
	require.Contains(t, strings.Join(warned, "\n"), "outside the bridge",
		"and the drop must be WARNED, never silent")
}

// TestApplyDiscoveredProposals_RejectsOneOfThreeSeeds is the floor on the
// proposal path, on a fixture where it is NOT confounded with the old
// all-seeds rule.
func TestApplyDiscoveredProposals_RejectsOneOfThreeSeeds(t *testing.T) {
	svc, branch, payload := threeSeedProposalEnv(t)

	props := []DiscoveredFact{{
		Path: "kb/thin.md", Title: "thin", Body: "thin", Type: "synthesis",
		Domain: []string{"x"}, Confidence: 0.9, Refs: []string{"kb/a.md"},
	}}

	written, err := applyDiscoveredProposals(context.Background(), svc.Facts(), svc.Search(),
		nil, payload, props, DiscoveryGates{}, branch, bareRefFixture, "kb", nil)
	require.NoError(t, err)
	require.Empty(t, written, "one citation is not a derivation path")
}

func readFactForTest(t *testing.T, svc *store.Service, branch, path string) fact.Fact {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, res.Content)
	require.NoError(t, err)
	return f
}

// ── the reinforce path, end to end ──

const seedThreePath = "kb/gotchas/sandboxing/thirdseed.md"

// threeSeedReinforceEnv extends the package's two-seed reinforce fixture with a
// third seed, so the subset rule can be told apart from the all-seeds rule it
// replaced. On the two-seed fixture the two rules coincide and no test built on
// it is evidence about #151 either way.
func threeSeedReinforceEnv(t *testing.T) *reinforceEnv {
	t.Helper()
	env := newReinforceEnv(t)

	f := fact.NewFact(seedThreePath)
	f.Title = "A third seed the target does not rest on"
	f.Body = "Body of the third seed."
	f.Type = fact.Observation
	f.Confidence = 0.8
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(context.Background(), env.branch, seedThreePath,
		content, "write third seed", "test")
	require.NoError(t, err)

	env.payload.Bridge.Members = append(env.payload.Bridge.Members,
		factForLLM{File: seedThreePath})
	return env
}

// TestApplyReinforcements_AcceptsTwoOfThreeSeeds is the relaxation on the
// reinforce path — the case the issue named as blocker 1. Reinforcement was
// near-unusable precisely because a bridge whose seeds do not ALL derive the
// target had no way to record the derivation that genuinely holds.
func TestApplyReinforcements_AcceptsTwoOfThreeSeeds(t *testing.T) {
	env := threeSeedReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement() // cites seedOne + seedTwo, not seedThree
	require.Equal(t, []string{reinforcePath}, env.apply(r),
		"two of three seeds IS an independent derivation and must be recorded")

	after := env.read(reinforcePath)
	require.Equal(t, before.Sources+1, after.Sources)
	require.NotContains(t, strings.Join(after.Refs, " "), seedThreePath,
		"the seed that does not fit must not be forced into the derivation path")
}

// TestApplyReinforcements_RejectsOneOfThreeSeeds is the floor, on the fixture
// where it is not confounded with the old rule.
func TestApplyReinforcements_RejectsOneOfThreeSeeds(t *testing.T) {
	env := threeSeedReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement()
	r.Refs = flexStrings{seedOnePath}
	require.Empty(t, env.apply(r))
	require.Equal(t, before.Sources, env.read(reinforcePath).Sources,
		"a rejected reinforcement must not move sources")
}

// TestApplyReinforcements_CanonicalSeedSpellingIsAccepted drives the real
// reinforce path with seeds cited the way the corpus stores them.
//
// It is the wiring half of TestSeedSubset_CanonicalSpellingCountsAsTheSeed: the
// predicate test proves the comparison normalises, this proves the CALLER hands
// it a real repo id. Before #151 this input failed twice over — rejected by the
// gate, and, had it passed, both seeds classified as extras and dropped, so the
// reinforcement would have been a no-op with sources untouched.
func TestApplyReinforcements_CanonicalSeedSpellingIsAccepted(t *testing.T) {
	env := threeSeedReinforceEnv(t)
	before := env.read(reinforcePath)

	r := goodReinforcement()
	r.Refs = flexStrings{
		fact.QualifyKBPath(env.repoID, seedOnePath),
		fact.QualifyKBPath(env.repoID, seedTwoPath),
	}
	require.Contains(t, string(r.Refs[0]), "kb://",
		"fixture check: bare paths here would not exercise normalisation")

	require.Equal(t, []string{reinforcePath}, env.apply(r),
		"seeds cited in the stored canonical spelling must be recognised")
	require.Equal(t, before.Sources+1, env.read(reinforcePath).Sources)
}
