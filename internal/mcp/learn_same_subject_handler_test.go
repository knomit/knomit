package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// Markers let a test pin the exact cosine between two facts. newLenEmbedder
// cannot: it keys on text LENGTH, so equal-length strings are cosine 1.0 and
// everything else is arbitrary — fine for driving the dedup floor, useless for
// landing a pair INSIDE a band.
const (
	seedMarker  = "SEEDMARKER"
	probeMarker = "PROBEMARKER"
)

// newAngleEmbedder places each marked text on a unit circle, so the cosine
// between any two marked texts is cos(angle difference) and a test can name the
// similarity it wants. Same trick the store's own helpers use to make
// neighbour ordering deterministic.
//
// th is what Thresholds() reports. It is a parameter because the shared harness
// pins params.Defaults() unconditionally, which would silently run every band
// case at the NOMIC geometry — the exact blind spot that let the original
// (SimilarTo, Dedup) band look correct.
func newAngleEmbedder(t *testing.T, modelID string, th params.Thresholds, angleByMarker map[string]float64) *MockBatchEmbedder {
	t.Helper()
	emb := NewMockBatchEmbedder(gomock.NewController(t))

	embed := func(text string) ([]float32, error) {
		// Unmarked text sits a quarter turn from every marker: cosine 0, well
		// below any band, so incidental facts never anchor a case.
		angle := math.Pi / 2
		for marker, a := range angleByMarker {
			if containsMarker(text, marker) {
				angle = a
				break
			}
		}
		out := make([]float32, 768)
		out[0] = float32(math.Cos(angle))
		out[1] = float32(math.Sin(angle))
		return out, nil
	}

	emb.EXPECT().EmbedQuery(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, text string) ([]float32, error) { return embed(text) }).AnyTimes()
	emb.EXPECT().EmbedDocument(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, title, body string) ([]float32, error) { return embed(title + " " + body) }).AnyTimes()
	emb.EXPECT().EmbedDocuments(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, titles, bodies []string) ([][]float32, error) {
			out := make([][]float32, len(titles))
			for i := range titles {
				out[i], _ = embed(titles[i] + " " + bodies[i])
			}
			return out, nil
		}).AnyTimes()
	emb.EXPECT().Dim().Return(768).AnyTimes()
	// The gate reads its band from params.ForModel keyed by THIS id, so a stub
	// id would turn the gate off rather than exercising it.
	emb.EXPECT().ID().Return(modelID).AnyTimes()
	emb.EXPECT().Thresholds().Return(th).AnyTimes()
	return emb
}

func containsMarker(text, marker string) bool {
	return len(text) >= len(marker) && indexOf(text, marker) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// angleFor returns the angle whose cosine against angle 0 is the given target.
func angleFor(cosine float64) float64 { return math.Acos(cosine) }

// newSameSubjectRepo wires a repo whose embedder reports th and places the seed
// at cosine 1.0 with itself and `probeCosine` with the probe fact.
func newSameSubjectRepo(t *testing.T, modelID string, th params.Thresholds, probeCosine float64) (*store.Service, context.Context, store.BatchEmbedder) {
	t.Helper()
	emb := newAngleEmbedder(t, modelID, th, map[string]float64{
		seedMarker:  0,
		probeMarker: angleFor(probeCosine),
	})
	return newRepoWithEmbedder(t, emb)
}

func newRepoWithEmbedder(t *testing.T, emb store.BatchEmbedder) (*store.Service, context.Context, store.BatchEmbedder) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	if emb != nil {
		svc.SetEmbedder(emb)
	}
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return svc, repos.WithRepoInstance(context.Background(), ri), emb
}

// learnReq builds a one-fact learn request. topic/category are separate so a
// test can put the two facts in DIFFERENT category directories, which is the
// whole point: today's dedup is category-scoped and cannot see across them.
func sameSubjectLearnReq(moment, topic, category, title, body string, entities []any, distinctFrom []any) mcpgo.CallToolRequest {
	f := map[string]any{
		"topic":      topic,
		"category":   category,
		"title":      title,
		"body":       body,
		"type":       "observation",
		"domain":     []any{},
		"confidence": 0.8,
		"sources":    1,
		"entities":   entities,
		"refs":       []any{},
	}
	if distinctFrom != nil {
		f["distinct_from"] = distinctFrom
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": moment, "facts": []any{f}}
	return req
}

// seedRampFact writes the existing fact every collision case collides with.
func seedRampFact(t *testing.T, ctx context.Context, emb store.BatchEmbedder) string {
	t.Helper()
	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
		"seed", "decisions", "accepted/ramp/ai-index",
		"Ramp AI Index shows adoption flat",
		"The "+seedMarker+" records Ramp's spend data on paid AI adoption.",
		[]any{"Ramp", "AI Index"}, nil))
	require.NoError(t, err)
	require.False(t, r.IsError, "seed write must succeed: %s", resultText(t, r))
	return mergedFactPath(t, r)
}

// liveFactCount reads how many facts are live on the branch, so a test can
// assert a refused call wrote NOTHING rather than merely returning an error.
func liveFactCount(t *testing.T, svc *store.Service) int {
	t.Helper()
	st, err := svc.FactQuery().Stats(context.Background(), "agent/test", "", "")
	require.NoError(t, err)
	return st.Total
}

// handlerThresholdSets mirrors thresholdSets but is used where the embedder
// itself must report the geometry.
func handlerThresholdSets(t *testing.T) []struct {
	name  string
	model string
	th    params.Thresholds
} {
	return thresholdSets(t)
}

// The collision this whole stage exists for: one event, two sessions, two
// category directories, differently worded. Category-scoped dedup cannot see
// it; the corpus-wide entity-anchored band can.
func TestLearnHandler_RefusesSameSubjectCollision(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seeded := seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"collide", "gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"}, nil))
			require.NoError(t, err)

			require.True(t, r.IsError, "a same-subject write must be refused; got %s", resultText(t, r))
			text := resultText(t, r)
			require.Contains(t, text, seeded, "the refusal must name the candidate's path")
			require.Contains(t, text, "Ramp", "the refusal must name the shared entity that anchored it")
			require.Contains(t, text, "knomit_update", "the refusal must say how to update the existing fact")
			require.Contains(t, text, "distinct_from", "the refusal must offer the escape")

			require.Equal(t, before, liveFactCount(t, svc),
				"a refused call must write NOTHING; the whole call is refused, never half of it")
		})
	}
}

// The escape hatch: the caller has looked at the candidate and asserts it is a
// different fact. Same setup as the refusal, one field added.
func TestLearnHandler_DistinctFromBypassesRefusal(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seeded := seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"assert-distinct", "gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"}, []any{seeded}))
			require.NoError(t, err)

			require.False(t, r.IsError, "distinct_from must let the write through: %s", resultText(t, r))
			require.Equal(t, before+1, liveFactCount(t, svc), "the asserted-distinct fact must be written")
		})
	}
}

// distinct_from names paths the caller claims to have checked. A path that does
// not exist means they checked nothing, so it is a validation error rather than
// a silently accepted bypass.
func TestLearnHandler_DistinctFromUnknownPathRejected(t *testing.T) {
	th := params.Defaults()
	svc, ctx, emb := newSameSubjectRepo(t, params.NomicModelID, th, inBand(th))
	seedRampFact(t, ctx, emb)
	before := liveFactCount(t, svc)

	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
		"bad-escape", "gotchas", "tools/ai/spend",
		"Enterprise AI spend is plateauing",
		"A "+probeMarker+" note on Ramp's numbers.",
		[]any{"Ramp"}, []any{"kb/nope.md"}))
	require.NoError(t, err)

	require.True(t, r.IsError, "an unknown distinct_from path must be rejected")
	require.Contains(t, resultText(t, r), "kb/nope.md")
	require.Equal(t, before, liveFactCount(t, svc), "a rejected call must write nothing")
}

// Regression guard on the existing path: a true near-duplicate in the SAME
// category is still auto-merged, not refused. The probe sits just above Dedup
// rather than at a comfortable 0.95, so the case actually exercises the real
// boundary for whichever model is in force.
func TestLearnHandler_AutoMergeStillWinsAboveDedup(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			justAbove := ts.th.Dedup + (1-ts.th.Dedup)*0.25
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, justAbove)
			seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"near-dup", "decisions", "accepted/ramp/ai-index",
				"Ramp AI Index shows adoption flat",
				"The "+probeMarker+" records Ramp's spend data on paid AI adoption.",
				[]any{"Ramp", "AI Index"}, nil))
			require.NoError(t, err)

			require.False(t, r.IsError, "an above-Dedup near-duplicate must merge, not be refused: %s", resultText(t, r))
			require.Equal(t, before, liveFactCount(t, svc), "the merge must land on the existing fact, not add one")
		})
	}
}

// No entities means no anchor, and text similarity alone is too noisy to
// refuse on. The probe is squarely in band; only the missing anchor saves it.
func TestLearnHandler_NoEntitiesNeverRefused(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"no-entities", "gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on the numbers.",
				[]any{}, nil))
			require.NoError(t, err)

			require.False(t, r.IsError, "a fact with no entities has no anchor and must be written: %s", resultText(t, r))
			require.Equal(t, before+1, liveFactCount(t, svc))
		})
	}
}

// With embeddings disabled EmbedderThresholds(nil) hands back the NOMIC band
// while no model is running, so the stage must not judge anything. Nothing is
// refused today only because an empty vector map makes the store return no rows
// before scoring — an accident. This pins the behaviour structurally.
func TestLearnHandler_NoEmbedderNeverRefused(t *testing.T) {
	svc, ctx, _ := newRepoWithEmbedder(t, nil)

	r1, err := LearnHandler()(ctx, sameSubjectLearnReq(
		"seed", "decisions", "accepted/ramp/ai-index",
		"Ramp AI Index shows adoption flat",
		"The "+seedMarker+" records Ramp's spend data.",
		[]any{"Ramp", "AI Index"}, nil))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed: %s", resultText(t, r1))
	before := liveFactCount(t, svc)

	r2, err := LearnHandler()(ctx, sameSubjectLearnReq(
		"no-embedder", "gotchas", "tools/ai/spend",
		"Enterprise AI spend is plateauing",
		"A "+probeMarker+" note on Ramp's numbers.",
		[]any{"Ramp"}, nil))
	require.NoError(t, err)

	require.False(t, r2.IsError, "with no embedder there is no band to judge against; write it: %s", resultText(t, r2))
	require.Equal(t, before+1, liveFactCount(t, svc))
}

// distinct_from must reach the schema, or a caller has no documented way to use
// the escape the refusal message tells them to use.
func TestLearnToolSchemaAdvertisesDistinctFrom(t *testing.T) {
	props := learnToolSchemaProperties()
	df, ok := props["distinct_from"]
	require.True(t, ok, "learn's schema must advertise distinct_from")

	blob, err := json.Marshal(df)
	require.NoError(t, err)
	require.Contains(t, string(blob), "array")
	require.Contains(t, string(blob), "knomit_update",
		"the description must point at the other resolution, not only the bypass")
}

// sameSubjectLearnReqMulti builds a MULTI-fact call. Every other case here is
// single-fact, and on a single-fact call "the whole call is refused, never half
// of it" is vacuous: an implementation that refused the colliding fact and wrote
// the innocent ones would pass all of them.
func sameSubjectLearnReqMulti(moment string, facts ...map[string]any) mcpgo.CallToolRequest {
	anyFacts := make([]any, len(facts))
	for i, f := range facts {
		anyFacts[i] = f
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": moment, "facts": anyFacts}
	return req
}

func sameSubjectFact(topic, category, title, body string, entities []any) map[string]any {
	return map[string]any{
		"topic": topic, "category": category, "title": title, "body": body,
		"type": "observation", "domain": []any{}, "confidence": 0.8, "sources": 1,
		"entities": entities, "refs": []any{},
	}
}

// One colliding fact poisons the WHOLE call, including facts that would have
// been written on their own. Facts in one learn call commit in one commit, so a
// partial write would report success for a batch that only half landed.
func TestLearnHandler_RefusalIsWholeCallNotPerFact(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seeded := seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			// fact 0 collides with nothing; fact 1 collides with the seed.
			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("mixed-batch",
				sameSubjectFact("gotchas", "runtime/unrelated",
					"An unrelated runtime trap", "Nothing to do with the other one.",
					[]any{"Goroutines"}),
				sameSubjectFact("gotchas", "tools/ai/spend",
					"Enterprise AI spend is plateauing",
					"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
					[]any{"Ramp"}),
			))
			require.NoError(t, err)
			require.True(t, r.IsError, "a batch containing a collision must be refused: %s", resultText(t, r))

			text := resultText(t, r)
			require.Contains(t, text, "fact 1", "the refusal must name the offending index")
			require.NotContains(t, text, "fact 0", "the innocent fact must not be reported as refused")
			require.Contains(t, text, seeded)

			require.Equal(t, before, liveFactCount(t, svc),
				"the INNOCENT fact must not be written either; one commit, all or nothing")
		})
	}
}

// The escape must survive the casing the validation already tolerates:
// FactExists lowercases, so a mixed-case path validates. If the bypass compared
// exactly, the caller would be refused again and told to do what they just did.
func TestLearnHandler_DistinctFromIsCaseInsensitive(t *testing.T) {
	th := params.Defaults()
	svc, ctx, emb := newSameSubjectRepo(t, params.NomicModelID, th, inBand(th))
	seeded := seedRampFact(t, ctx, emb)
	before := liveFactCount(t, svc)

	shouted := strings.ToUpper(seeded)
	require.NotEqual(t, seeded, shouted, "the case variant must actually differ")

	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
		"case-variant", "gotchas", "tools/ai/spend",
		"Enterprise AI spend is plateauing",
		"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
		[]any{"Ramp"}, []any{shouted}))
	require.NoError(t, err)

	require.False(t, r.IsError,
		"a distinct_from path that PASSES validation must also satisfy the bypass: %s", resultText(t, r))
	require.Equal(t, before+1, liveFactCount(t, svc))
}

// captureLogs swaps the global logger for a buffer, the house pattern (see
// internal/repos/manager_session_db_test.go).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

// B1's second gate-off exit: an embedder whose model params does not know has
// no calibrated band, so the gate turns OFF rather than judging against some
// other model's geometry.
//
// This asserts the LOG EVENT, not just that the write landed, and the
// distinction is the whole point. Deleting the `!ok` check while keeping the
// lookup leaves `th` as the zero Thresholds, which makes `cosine >= th.Dedup`
// true for every candidate — so the gate-off OUTCOME survives by numeric
// accident, exactly the shape of the bug this exit was added to remove. What is
// genuinely lost is the warning: a refactor could drop the guard, keep the
// behaviour, keep this test green, and silently destroy the only signal that
// the gate is off. Pinning the log pins the mechanism.
func TestLearnHandler_UnknownModelTurnsTheGateOff(t *testing.T) {
	th := params.Defaults()
	// A working embedder reporting an id params has no calibration for.
	const unknownModel = "no-such-model-v9"
	emb := newAngleEmbedder(t, unknownModel, th, map[string]float64{
		seedMarker: 0, probeMarker: angleFor(inBand(th)),
	})
	svc, ctx, _ := newRepoWithEmbedder(t, emb)
	seedRampFact(t, ctx, emb)
	before := liveFactCount(t, svc)

	logs := captureLogs(t)
	r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
		"unknown-model", "gotchas", "tools/ai/spend",
		"Enterprise AI spend is plateauing",
		"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
		[]any{"Ramp"}, nil))
	require.NoError(t, err)

	require.False(t, r.IsError,
		"no calibrated band for this model means no gate, not a refusal: %s", resultText(t, r))
	require.Equal(t, before+1, liveFactCount(t, svc))

	out := logs.String()
	require.Contains(t, out, "no calibrated thresholds for this embedding model",
		"turning the gate off MUST be announced; without the warning a disabled gate is invisible")
	require.Contains(t, out, unknownModel,
		"the warning must name the model, or it cannot be acted on")
}

// B2 limb (a), ALONE: refs cite the candidate and Origin is EMPTY. This is the
// limb that actually carries pipeline output, because learn leaves Origin "" on
// the struct unless the caller passed it. A test that set both origin and refs
// would pass with this limb broken.
func TestLearnHandler_RefsCitedCandidateSkippedWithEmptyOrigin(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seeded := seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			f := sameSubjectFact("gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"})
			f["refs"] = []any{seeded} // declared lineage
			// origin deliberately unset: this must pass on refs alone.

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("cited", f))
			require.NoError(t, err)
			require.False(t, r.IsError,
				"a candidate the fact already cites is declared lineage, not a collision: %s", resultText(t, r))
			require.Equal(t, before+1, liveFactCount(t, svc))
		})
	}
}

// B2 limb (b), ALONE: origin is pipeline output and refs do NOT cite the
// candidate. Proves the origin limb works without the refs limb covering for it.
func TestLearnHandler_PipelineOriginExemptWithoutRefs(t *testing.T) {
	for _, ts := range handlerThresholdSets(t) {
		t.Run(ts.name, func(t *testing.T) {
			svc, ctx, emb := newSameSubjectRepo(t, ts.model, ts.th, inBand(ts.th))
			seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			f := sameSubjectFact("gotchas", "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"})
			// origin distilled is only valid on a synthesis fact; the
			// serializer enforces the pairing.
			f["origin"] = string(fact.Distilled)
			f["type"] = "synthesis"
			f["refs"] = []any{} // no lineage cited: the origin limb must carry it

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReqMulti("pipeline", f))
			require.NoError(t, err)
			require.False(t, r.IsError,
				"pipeline output is exempt; it collides with its sources by construction: %s", resultText(t, r))
			require.Equal(t, before+1, liveFactCount(t, svc))
		})
	}
}
