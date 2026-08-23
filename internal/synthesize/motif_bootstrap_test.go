package synthesize

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// FROM NOTHING (roadmap lesson 7).
//
// Every other motif test in this repo — around sixty of them — calls
// RebuildAliases by hand as its first act. That makes each one a test of a
// component given state it was handed, and NONE of them can see whether the
// system ever builds that state itself. It did not: the only production caller
// of RebuildAliases was the apply path of the judge work item, the judge item
// was planned only when the vocabulary exceeded a floor, and the vocabulary was
// read from the table RebuildAliases writes. A closed loop with no entry.
//
// This test starts from an empty store, writes facts the way a user does, runs
// a REAL review session, and asks whether the system built its own state. It is
// the only test here that could have caught that, and it is deliberately first
// in the file.
//
// The rule it encodes: a derived-state suite needs at least one test that hands
// the system NOTHING. Hand-seeded fixtures structurally cannot detect a
// bootstrap deadlock, because seeding IS the thing that never happens.

func TestBootstrap_SessionBuildsItsOwnAliasTableFromNothing(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	// Twenty facts carrying twenty motifs, written exactly as a user would.
	// NOTHING is pre-built: no RebuildAliases, no seeded alias rows.
	for i := range 20 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i),
			fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}

	before, err := env.svc.Motifs().AliasTable(ctx, env.branch)
	require.NoError(t, err)
	require.Empty(t, before, "precondition: nothing has built the alias table yet")

	// One real session, at an effort that maintains the vocabulary.
	out := env.vocabSession()
	require.NotEmpty(t, out.sessionID)

	after, err := env.svc.Motifs().AliasTable(ctx, env.branch)
	require.NoError(t, err)
	require.NotEmpty(t, after,
		"a review session must BUILD the mechanical alias layer. Nothing else can: "+
			"the judge pass is gated on a vocabulary that only this rebuild produces, "+
			"so a system that waits for the judge waits forever.")
	require.Len(t, after, 20, "every authored motif must resolve")
}

// The consumers must come alive once the system builds its own state. Each of
// these silently degrades to a correct-looking zero when the table is empty,
// which is why the deadlock was invisible.
func TestBootstrap_ConsumersComeAliveAfterOneSession(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// A corpus with real aliasing in it: two spellings of one mechanism.
	env.writeFactWithMotifs("kb/a.md", "Alpha", "a distinct body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "another distinct body", []string{"silent-fallbacks"})
	for i := range 14 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}

	env.vocabSession()

	// Alias resolution: the two spellings are ONE cluster.
	a, err := env.svc.Motifs().CanonicalID(ctx, env.branch, "silent-fallback")
	require.NoError(t, err)
	b, err := env.svc.Motifs().CanonicalID(ctx, env.branch, "silent-fallbacks")
	require.NoError(t, err)
	require.Equal(t, a, b, "aliasing must work after one session, with no hand-seeding")

	// TokenDF counts the cluster, not the spelling.
	df, err := env.svc.Search().TokenDF(ctx, env.branch, a, "motif")
	require.NoError(t, err)
	require.Equal(t, 2, df, "df must span the cluster — the singleton fallback reads 1")

	// Definitions have something to queue.
	need, err := env.svc.Motifs().ClustersNeedingDefinition(ctx, env.branch)
	require.NoError(t, err)
	require.NotEmpty(t, need,
		"an unresolved vocabulary queues NOTHING for definition, which looks exactly "+
			"like a fully-defined corpus")

	// Health reports a real vocabulary rather than a permanent below-floor.
	vh, err := env.svc.Motifs().VocabularyHealth(ctx, env.branch)
	require.NoError(t, err)
	require.Positive(t, vh.Clusters)
	require.Positive(t, vh.Recurring, "the aliased pair is the corpus's recurrence")
}

// C2: every motif work item must SHIP ITS PAYLOAD. All three prompts tell the
// model its data rides in the work item's facts field; all three renderers
// omitted it, so the judge decided pairs it was never shown and the definer
// wrote definitions for names it never received.
//
// Asserted through the real render path, per step type, because that is where
// the omission lived — no test opened it before.
func TestBootstrap_EveryMotifWorkItemShipsItsPayload(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "a distinct body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "another distinct body", []string{"silent-fallbacks"})
	for i := range 16 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	env.writeFact("kb/bare.md", "Bare fact", "a body with no motif")

	out := env.vocabSession()

	seen := map[string]bool{}
	for _, item := range out.restatementItems {
		switch item.StepType {
		case motifAliasStepType, motifDefineStepType, motifBackfillStepType:
			seen[item.StepType] = true
			sess := pipelineSessionFor(env, out.sessionID)
			view, err := (reviewStrategy{}).Render(ctx, env.deps(), &sess, &item)
			require.NoErrorf(t, err, "render %s", item.StepType)
			require.NotEmptyf(t, view.Facts,
				"%s ships an EMPTY payload while its prompt says the data rides in the "+
					"work item's facts field — the model is asked to decide about content "+
					"it never receives", item.StepType)
			require.JSONEqf(t, item.FactsJSON, view.Facts,
				"%s must ship exactly the payload it was planned with", item.StepType)
		}
	}
	require.NotEmpty(t, seen, "the session must plan at least one motif item to check")
}

// pipelineSessionFor rebuilds the session record Render needs.
func pipelineSessionFor(env *restatementEnv, id string) store.PipelineSession {
	return store.PipelineSession{ID: id, Branch: env.branch}
}

// What the ENGINE SERVES, not what Render returns.
//
// C2 was "the renderer shipped no payload". This is its sibling one layer
// deeper: the renderer shipped the right payload and the PAGING layer replaced
// it with blanks on the way out.
//
// factPages unmarshalled any payload into []factForLLM to split it into pages.
// That SUCCEEDS for any JSON array of objects — Go ignores unknown fields — so
// the alias and define payloads (both arrays) decoded to slices of EMPTY
// structs and were re-marshalled over the real content. Backfill survived only
// because its payload is an object, where the unmarshal fails and the verbatim
// fall-through catches it.
//
// Every layer looked correct in isolation: Render returned the payload, the
// item stored the payload, and only the served result was wrong. The C2 test
// asserted Render's output and passed throughout.
func TestBootstrap_EngineServesTheRealPayloadNotABlankedOne(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithMotifs("kb/a.md", "Alpha", "a distinct body", []string{"silent-fallback"})
	env.writeFactWithMotifs("kb/b.md", "Bravo", "another distinct body", []string{"silent-fallbacks"})
	for i := range 16 {
		env.writeFactWithMotifs(
			fmt.Sprintf("kb/f%02d.md", i), fmt.Sprintf("Fact %d", i),
			fmt.Sprintf("A distinct body about subject number %d.", i),
			[]string{fmt.Sprintf("mechanism-%s", numberWord(i))})
	}
	env.writeFact("kb/bare.md", "Bare fact", "a body with no motif")

	rv := NewReviewerWithOptions(env.ri, nil, EffortMedium, ScopeFilter{})
	res, err := rv.StartSession(ctx)
	require.NoError(t, err)

	checked := map[string]bool{}
	for range 200 {
		if res.Done || res.Item == nil {
			break
		}
		if motifStepForTest(res.Item.Type) {
			checked[res.Item.Type] = true
			// The payload the MODEL receives must carry real content, not a
			// well-formed shell. Emptiness here is invisible to every other
			// check: the JSON is valid, the field is present, the length is
			// plausible.
			require.NotEmptyf(t, res.Item.Facts, "%s served an empty payload", res.Item.Type)
			require.NotContainsf(t, string(res.Item.Facts), `"path":"","title":"","body":""`,
				"%s served BLANKED factForLLM structs — the paging layer replaced the "+
					"real payload with empty ones", res.Item.Type)
			switch res.Item.Type {
			case motifAliasStepType:
				require.Contains(t, string(res.Item.Facts), `"a_carriers"`,
					"the judge must receive carrier titles — they are the over-merge guard")
			case motifDefineStepType:
				require.Contains(t, string(res.Item.Facts), `"cluster_key"`,
					"the definer's answer must be routable back to a cluster")
			case motifBackfillStepType:
				require.Contains(t, string(res.Item.Facts), `"vocabulary"`)
			}
		}
		res, err = rv.ContinueSessionForItem(ctx, res.SessionID, emptyResponseForTest(res.Item.Type), res.Item.ID)
		if err != nil {
			// A paged ordinary item needs its token; not this test's subject.
			break
		}
	}
	require.Containsf(t, checked, motifDefineStepType,
		"the fixture must serve a define item — it is the payload that was being blanked")
}

func motifStepForTest(t string) bool {
	switch t {
	case motifAliasStepType, motifDefineStepType, motifBackfillStepType:
		return true
	}
	return false
}

func emptyResponseForTest(stepType string) string {
	switch stepType {
	case motifAliasStepType:
		return `{"verdicts":[]}`
	case motifDefineStepType:
		return `{"definitions":[]}`
	case motifBackfillStepType:
		return `{"assignments":[]}`
	case "prune":
		return `{"decisions":[],"merges":[]}`
	case "distill":
		return `{"synthesize":[],"retract":[]}`
	case "reflect":
		return `{"reasoning":"x","reinforce":[],"propose":[]}`
	case "discover":
		return `{"proposals":[]}`
	}
	return `{}`
}
