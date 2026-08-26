package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// ── the §11 subtraction residue ───────────────────────────────────────────

// The residue is title tokens MINUS subject tokens. What remains is, by
// construction, the words the author used that are NOT about the subject.
func TestSubtractionResidue_RemovesSubjectTokens(t *testing.T) {
	got := subtractionResidue(store.BackfillTarget{
		Path:     "kb/store/caching/abc.md",
		Title:    "Postgres connection pool exhausts under retry storms",
		Domain:   []string{"store", "caching"},
		Entities: []string{"Postgres"},
	})
	joined := strings.Join(got, " ")
	require.NotContains(t, joined, "postgres", "an entity is subject, not aspect")
	require.NotContains(t, joined, "store", "a domain is subject")
	require.NotContains(t, joined, "caching", "a path token is subject")
	require.Contains(t, joined, "exhaust", "the mechanism words are what remain")
	require.Contains(t, joined, "retry")
}

// A fact whose title says nothing beyond its subject has an EMPTY residue —
// and that is the correct answer, not a failure. It is also the common case
// that made subtraction fail as a product (§11) and survive only as a hint.
func TestSubtractionResidue_SubjectOnlyTitleYieldsNothing(t *testing.T) {
	got := subtractionResidue(store.BackfillTarget{
		Path:     "kb/store/postgres.md",
		Title:    "Postgres",
		Entities: []string{"Postgres"},
	})
	require.Empty(t, got)
}

// ── the vocabulary hint ───────────────────────────────────────────────────

// Hints are df >= 2 only. A hapax motif is one author's coinage that nothing
// has reused; offering it would spread a name the corpus has not endorsed.
func TestBackfillHints_ExcludeHapaxMotifs(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// A recurring motif (2 carriers) and a hapax.
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/c.md", "Charlie", "body", []string{"lonely-coinage"})
	env.writeFact("kb/target.md", "Target", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	targets, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, 8)
	require.NoError(t, err)
	require.NotEmpty(t, targets)

	hints := buildBackfillHints(ctx, env.deps(), env.branch, targets)
	for _, h := range hints {
		require.GreaterOrEqual(t, h.DF, 2,
			"hint %q has df %d — a hapax is one author's coinage, and offering it "+
				"would spread a name the corpus has not endorsed", h.Motif, h.DF)
		require.NotEqual(t, "lonely-coinage", h.Motif)
	}
}

// ── target selection ──────────────────────────────────────────────────────

// Only AUTHORED facts are offered. A derived fact without motifs is the
// pipeline having decided it needed none, and re-asking would be the engine
// second-guessing itself rather than filling a gap a human left.
func TestBackfillTargets_ExcludeDerivedFacts(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/authored.md", "Authored", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	targets, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, 8)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "kb/authored.md", targets[0].Path)
}

// A fact that already has motifs is not a backfill target.
func TestBackfillTargets_SkipFactsThatAlreadyHaveMotifs(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/has.md", "Has", "body", []string{"silent-fallback"})
	env.writeFact("kb/lacks.md", "Lacks", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	targets, err := env.svc.Motifs().LiveFactsWithoutMotifs(ctx, env.branch, 8)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "kb/lacks.md", targets[0].Path)
}

// ── response handling ─────────────────────────────────────────────────────

func backfillOffered() backfillPayload {
	return backfillPayload{Facts: []backfillItem{{Path: "kb/a.md"}, {Path: "kb/b.md"}}}
}

// Backfill REWRITES facts, so an invented path would put motifs on a fact
// nobody asked about.
func TestBackfillDecode_RejectsAnUnofferedPath(t *testing.T) {
	err := validateMotifBackfill(motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/never-offered.md", Motifs: []string{"silent-fallback"}},
	}}, backfillOffered())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not offered")
}

func TestBackfillDecode_RejectsDuplicateAssignments(t *testing.T) {
	err := validateMotifBackfill(motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/a.md", Motifs: []string{"one-motif-here"}},
		{Path: "kb/a.md", Motifs: []string{"other-motif-here"}},
	}}, backfillOffered())
	require.Error(t, err)
	require.Contains(t, err.Error(), "more than once")
}

// An empty assignment is a legitimate answer — many facts instantiate no
// general regularity — and must not be a validation failure.
func TestBackfillDecode_EmptyAssignmentIsAllowed(t *testing.T) {
	require.NoError(t, validateMotifBackfill(motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/a.md", Motifs: []string{}}},
	}, backfillOffered()))
}

func TestBackfillDecode_WrongEnvelopeKeyIsLoud(t *testing.T) {
	_, err := parseMotifBackfillResponse(`{"decisions":[{"path":"kb/a.md","motifs":[]}]}`)
	require.Error(t, err,
		"assignments is an ENVELOPE key — unlike a nested motifs field, its absence "+
			"means the response arrived under another name and would apply as a no-op")
	_, err = parseMotifBackfillResponse(`{"assignments":[]}`)
	require.NoError(t, err)
}

// ── apply ─────────────────────────────────────────────────────────────────

// Backfill writes through SerializeFact (MN4), so the subject strip applies
// exactly as it does to a hand-authored fact — backfill invents no second gate.
func TestBackfillApply_SubjectMotifIsStrippedByTheOneGate(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/alpha/one.md", "Widget behaviour", "body", nil)
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	// "widget-alpha" is entity ∪ domain for this fixture's writer.
	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/alpha/one.md", Motifs: []string{"widget-alpha", "silent-fallback"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, env.deps(), sessionForBackfillTest(t, ctx, env), env.branch, res, offeredBackfillForTest(t, ctx, env)))

	rec, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.Equal(t, []string{"silent-fallback"}, rec.Motifs,
		"the subject-restating motif is dropped by the single write gate, not by "+
			"backfill-specific code")
}

// A fact that gained motifs since the item was rendered is SKIPPED. Something
// else answered the question this item was asking, and the fresher answer wins.
func TestBackfillApply_SkipsAFactThatGainedMotifsMeanwhile(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/alpha/one.md", "Alpha", "body", []string{"already-here-now"})
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	res := motifBackfillResult{Assignments: []motifAssignment{
		{Path: "kb/alpha/one.md", Motifs: []string{"silent-fallback"}},
	}}
	require.NoError(t, applyMotifBackfill(ctx, env.deps(), sessionForBackfillTest(t, ctx, env), env.branch, res, offeredBackfillForTest(t, ctx, env)))

	rec, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.Equal(t, []string{"already-here-now"}, rec.Motifs,
		"backfill must not overwrite an answer that arrived while its item was outstanding")
}

// An empty assignment writes NOTHING — not an empty motif list onto the fact,
// which would be a pointless new revision of every fact the pass declined.
func TestBackfillApply_EmptyAssignmentWritesNothing(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/alpha/one.md", "Alpha", "body")
	before, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)

	require.NoError(t, applyMotifBackfill(ctx, env.deps(), sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{{Path: "kb/alpha/one.md", Motifs: []string{}}},
	}, offeredBackfillForTest(t, ctx, env)))

	after, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.Equal(t, before.BlobHash, after.BlobHash,
		"declining to motif a fact must not rewrite it")
}

// ── the prompt ────────────────────────────────────────────────────────────

// The two E2-validated write rules the designer added, and the framings that
// carry the asymmetry.
func TestBackfillPrompt_CarriesTheWriteRules(t *testing.T) {
	body, err := os.ReadFile("prompts/large/motif_backfill_user.txt")
	require.NoError(t, err)
	text := string(body)

	require.Contains(t, text, "PREFER AN EXISTING NAME")
	require.Contains(t, text, "ZERO IS THE RIGHT ANSWER OFTEN")
	require.Contains(t, text, "A wrong motif is worse than none")
	// Anti-remedy: E2 measured a unanimous failure without this rule.
	require.Contains(t, text, "names the fix rather than the failure")
	require.Contains(t, text, "unmonitored-expiry")
	// Intra-fact synonyms, live given the 3-slot budget.
	require.Contains(t, text, "two phrasings of the same regularity")
	// The residue must be presented as ignorable, or §11's product failure is
	// reintroduced inside the hint.
	require.Contains(t, text, "never as an answer")
	// Topic vs mechanism, the failure backfill invites most.
	require.Contains(t, text, "is a topic")
}

// ── health ────────────────────────────────────────────────────────────────

func TestBackfillHealth_ReportsCoverage(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "body", []string{"silent-fallback"})
	env.writeFact("kb/b.md", "Bravo", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)
	sess.Health = []string{"another producer's line"}
	require.NoError(t, planMotifBackfillWork(ctx, env.deps(), sess, env.branch))

	joined := strings.Join(sess.Health, "\n")
	require.Contains(t, joined, "motif backfill: coverage 50%")
	require.Contains(t, joined, "another producer's line", "appended, not substituted")
}

// The payload carries the vocabulary — this is the one item that legitimately
// does — and the residue hints.
func TestBackfillWorkItem_PayloadCarriesVocabularyAndResidue(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha exhausts under load", "body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo exhausts under load", "body", []string{"silent-fallback"})
	env.writeFact("kb/target.md", "Target exhausts under retry storms", "body")
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch)
	require.NoError(t, err)
	require.NoError(t, planMotifBackfillWork(ctx, env.deps(), sess, env.branch))

	item, err := env.svc.Pipeline().NextPipelineWorkItem(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, item)
	require.Equal(t, motifBackfillStepType, item.StepType)

	var payload backfillPayload
	require.NoError(t, json.Unmarshal([]byte(item.FactsJSON), &payload))
	require.NotEmpty(t, payload.Facts)
	var target *backfillItem
	for i := range payload.Facts {
		if payload.Facts[i].Path == "kb/target.md" {
			target = &payload.Facts[i]
		}
	}
	require.NotNil(t, target)
	require.NotEmpty(t, target.Residue, "the §11 residue hint must ride with the fact")
}

// Backfill ADDS MOTIFS AND CHANGES NOTHING ELSE.
//
// This is the guard for a bug that was live in the first draft and would have
// destroyed data. The apply path parsed FactWithBody.Body — which is the
// content BELOW the frontmatter — so the parsed fact had no title, no domain,
// no entities and no refs, and serializing it back replaced a real fact with a
// husk carrying only the new motifs.
//
// Nothing about that is visible from the motifs field alone, which is why the
// assertion here is about everything EXCEPT motifs. It is the second time in
// this phase that body-vs-source was confused; in the hint generator it merely
// produced a useless residue, and here it silently deletes authored content.
func TestBackfillApply_PreservesEveryOtherField(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/alpha/one.md", "A precise title", "A precise body.", nil)
	require.NoError(t, env.svc.Motifs().RebuildAliases(ctx, env.branch))

	before, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.NotEmpty(t, before.Title)
	require.NotEmpty(t, before.Domain, "fixture must carry the fields whose loss we are guarding")
	require.NotEmpty(t, before.Entities)

	require.NoError(t, applyMotifBackfill(ctx, env.deps(), sessionForBackfillTest(t, ctx, env), env.branch, motifBackfillResult{
		Assignments: []motifAssignment{
			{Path: "kb/alpha/one.md", Motifs: []string{"silent-fallback"}},
		},
	}, offeredBackfillForTest(t, ctx, env)))

	after, err := env.svc.FactQuery().GetByPath(ctx, env.branch, "kb/alpha/one.md")
	require.NoError(t, err)
	require.Equal(t, []string{"silent-fallback"}, after.Motifs, "the motif landed")
	require.Equal(t, before.Title, after.Title, "backfill must not touch the title")
	require.Equal(t, before.Domain, after.Domain, "...nor the domain")
	require.Equal(t, before.Entities, after.Entities, "...nor the entities")
	require.Equal(t, before.Type, after.Type, "...nor the type")
	require.Equal(t, before.Confidence, after.Confidence, "...nor the confidence")
	require.Contains(t, after.Body, "A precise body", "...nor the body")
}

// ── the effort gate ───────────────────────────────────────────────────────

// EffortNormal runs NO motif vocabulary pass. Not a tuning choice: all three
// spend LLM budget, and EffortNormal's contract is zero discovery spend with
// byte-identical output (MN5).
//
// Backfill is what makes this concrete rather than theoretical. It fires on any
// authored fact lacking a motif — which is EVERY fact on a motif-free corpus,
// exactly the corpus MN5's test uses. An ungated pass does not merely add cost
// to a normal-effort session, it changes what that session produces. Four
// unrelated pipeline tests failed the moment the passes were wired without
// this gate, which is how it was found.
func TestMotifPasses_NeverRunAtEffortNormal(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// A corpus where all three passes would have plenty to do.
	for i := range 18 {
		env.writeFactWithMotifs(fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	env.writeFact("kb/bare.md", "Bare", "a body with no motif")

	// Through PLAN, which is what gates. The first version of this test
	// asserted MaintainsVocabulary() twice and never ran the pipeline — so it
	// tested the predicate, which was never in doubt, and not the gate, which
	// was the thing that had been missing.
	normal := NewReviewerWithOptions(env.ri, nil, EffortNormal, ScopeFilter{})
	res, err := normal.StartSession(ctx)
	require.NoError(t, err)

	for _, item := range env.workItems(res.SessionID) {
		require.NotContainsf(t,
			[]string{motifAliasStepType, motifDefineStepType, motifBackfillStepType},
			item.StepType,
			"EffortNormal planned a %s item. All three passes spend LLM budget, and "+
				"EffortNormal guarantees zero discovery spend with byte-identical output "+
				"(MN5) — backfill alone would fire on every fact of a motif-free corpus, "+
				"which is exactly the corpus MN5's test uses.", item.StepType)
	}

	// And the alias table stays untouched, since the rebuild is inside the gate.
	aliases, err := env.svc.Motifs().AliasTable(ctx, env.branch)
	require.NoError(t, err)
	require.Empty(t, aliases, "EffortNormal must not build derived vocabulary either")

	// With the gate open, the same corpus DOES produce the passes — otherwise
	// this test would pass against a build where they had been deleted.
	medium := env.vocabSession()
	var planned int
	for _, item := range medium.restatementItems {
		switch item.StepType {
		case motifAliasStepType, motifDefineStepType, motifBackfillStepType:
			planned++
		}
	}
	require.Positive(t, planned,
		"at medium effort the same corpus must produce motif work — a gate that is "+
			"closed at every level is indistinguishable from a feature that was removed")
}
