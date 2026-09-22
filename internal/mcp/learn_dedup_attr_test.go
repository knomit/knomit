package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// dedupAttrOntologyYAML flags `inbox` (and so every category under it) with
// learn_dedup: off, and leaves `notes` as the unflagged control. `decisions`
// and `gotchas` are there for the same-subject fixtures, which seed under
// decisions.
const dedupAttrOntologyYAML = `id: t
name: T
topics:
  inbox:
    description: x
    attributes:
      learn_dedup: off
  notes:
    description: x
  decisions:
    description: x
  gotchas:
    description: x
`

func dedupAttrOntology(t *testing.T) *fact.Ontology {
	t.Helper()
	o, err := fact.ParseOntology([]byte(dedupAttrOntologyYAML))
	require.NoError(t, err)
	return o
}

// newDedupAttrRepo is newPrinciplesTestRepo with the flagged ontology: the
// length embedder makes identical text land at cosine 1.0, far above the
// dedup floor. Returns the RepoInstance too, for tests that drive
// applyDedupMerge directly.
func newDedupAttrRepo(t *testing.T) (*repos.RepoInstance, context.Context, store.BatchEmbedder) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := newLenEmbedder(t)
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     dedupAttrOntology(t),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return ri, repos.WithRepoInstance(context.Background(), ri), emb
}

// taskFactReq is one templated protocol-style message — the shape F02 exists
// for: two tasks in one directory whose text is near-identical.
func taskFactReq(topic string) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "task",
		"facts": []any{map[string]any{
			"topic":      topic,
			"category":   "queue/pending",
			"title":      "Task ready for pickup",
			"body":       "A task is ready for an agent to pick up.",
			"type":       "observation",
			"domain":     []any{"queue"},
			"confidence": 0.8,
			"sources":    1,
			"entities":   []any{"task-queue"},
			"refs":       []any{},
		}},
	}
	return req
}

func TestLearnHandler_LearnDedupOffWritesBothFacts(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)

	r1, err := LearnHandler(emb)(ctx, taskFactReq("inbox"))
	require.NoError(t, err)
	require.False(t, r1.IsError, resultText(t, r1))
	first := mergedFactPath(t, r1)

	r2, err := LearnHandler(emb)(ctx, taskFactReq("inbox"))
	require.NoError(t, err)
	require.False(t, r2.IsError, resultText(t, r2))
	second := mergedFactPath(t, r2)

	require.NotEqual(t, first, second,
		"under learn_dedup: off two identical facts must land as TWO files, not merge")
	svc := testRepoService(t, ctx)
	for _, p := range []string{first, second} {
		ok, err := svc.Facts().FactExists(context.Background(), "agent/test", p)
		require.NoError(t, err)
		require.True(t, ok, "%s must exist on the branch", p)
	}
	require.Equal(t, 2, liveFactCount(t, svc))
}

// The control: the same two facts into an unflagged topic merge into one. If
// this stops holding, the test above proves nothing.
func TestLearnHandler_UnflaggedTopicStillMerges(t *testing.T) {
	_, ctx, emb := newDedupAttrRepo(t)

	r1, err := LearnHandler(emb)(ctx, taskFactReq("notes"))
	require.NoError(t, err)
	require.False(t, r1.IsError, resultText(t, r1))
	first := mergedFactPath(t, r1)

	r2, err := LearnHandler(emb)(ctx, taskFactReq("notes"))
	require.NoError(t, err)
	require.False(t, r2.IsError, resultText(t, r2))
	require.Equal(t, first, mergedFactPath(t, r2), "the second write must merge into the first")
	require.Equal(t, 1, liveFactCount(t, testRepoService(t, ctx)))
}

// applyDedupMerge directly, so touched and the embedding donation are visible.
// A flagged fact skips the search but KEEPS its donation: it is written at its
// own path and indexed, so its vector is valid — unlike a private-state fact,
// which is never indexed and has its donation dropped.
func TestApplyDedupMerge_LearnDedupOffSkipsSearchKeepsDonation(t *testing.T) {
	ri, ctx, emb := newDedupAttrRepo(t)
	for _, topic := range []string{"inbox", "notes"} {
		r, err := LearnHandler(emb)(ctx, taskFactReq(topic))
		require.NoError(t, err)
		require.False(t, r.IsError, resultText(t, r))
	}

	conf, src := 0.8, 1
	in := func(topic string) learnFactInput {
		return learnFactInput{
			Topic: topic, Category: "queue/pending",
			Title: "Task ready for pickup", Body: "A task is ready for an agent to pick up.",
			Type: "observation", Domain: []string{"queue"}, Confidence: &conf, Sources: &src,
			Entities: []string{"task-queue"},
		}
	}
	private := learnFactInput{
		Path:  ".knomit/jobs/state.md",
		Title: "Task ready for pickup", Body: "A task is ready for an agent to pick up.",
		Type: "observation", Confidence: &conf, Sources: &src,
	}
	inputs := []learnFactInput{in("inbox"), in("notes"), private}

	ont := ri.Ontology()
	facts, tcs, paths, files, err := validateAndBuildFacts(ont, "kb", inputs)
	require.NoError(t, err)
	minted := append([]string(nil), paths...)

	s, release, err := storeIndices(ri)
	require.NoError(t, err)
	defer release()
	vecs := dedupEmbed(ctx, emb, facts)
	embByPath, _, _, touched, err := applyDedupMerge(ctx, s, "agent/test", ont, emb, vecs, facts, tcs, paths, files, "x")
	require.NoError(t, err)

	// Flagged: not touched, path unchanged, donation kept under its own path.
	require.False(t, touched[0], "a flagged fact must not be merged")
	require.Equal(t, minted[0], paths[0], "a flagged fact keeps its freshly-minted path")
	require.Contains(t, embByPath, paths[0], "a flagged fact still donates its vector")

	// Unflagged control: merged into the seed.
	require.True(t, touched[1], "the unflagged fact must take the merge path")
	require.NotEqual(t, minted[1], paths[1], "the merge retargets to the existing fact")

	// Private state: no merge, and the donation is dropped.
	require.False(t, touched[2])
	require.NotContains(t, embByPath, paths[2], "private state never donates")
}

// testRepoService pulls the store back out of a handler ctx.
func testRepoService(t *testing.T, ctx context.Context) *store.Service {
	t.Helper()
	svc, release, err := repos.RepoFromContext(ctx).Acquire()
	require.NoError(t, err)
	t.Cleanup(release)
	return svc
}

// Same-subject: an incoming fact under a flagged topic that shares a
// non-generic entity with an in-band candidate is NOT refused. The unflagged
// row is the control on the same fixture — it must still be refused, as
// TestLearnHandler_RefusesSameSubjectCollision establishes on CodeOntology.
func TestLearnHandler_SameSubjectSkipsLearnDedupOffTopic(t *testing.T) {
	for _, tc := range []struct {
		topic   string
		refused bool
	}{
		{"inbox", false},
		{"gotchas", true},
	} {
		t.Run(tc.topic, func(t *testing.T) {
			th, ok := params.ForModel(params.NomicModelID)
			require.True(t, ok)
			angle := newAngleEmbedder(t, params.NomicModelID, th, map[string]float64{
				seedMarker:  0,
				probeMarker: angleFor(inBand(th)),
			})
			svc, ctx, emb := newRepoWithEmbedderOntology(t, angle, dedupAttrOntology(t))
			seedRampFact(t, ctx, emb)
			before := liveFactCount(t, svc)

			r, err := LearnHandler(emb)(ctx, sameSubjectLearnReq(
				"collide", tc.topic, "tools/ai/spend",
				"Enterprise AI spend is plateauing",
				"A "+probeMarker+" note on Ramp's numbers for paid AI seats.",
				[]any{"Ramp"}, nil))
			require.NoError(t, err)
			if tc.refused {
				require.True(t, r.IsError, "control: an unflagged topic must still be refused; got %s", resultText(t, r))
				require.Equal(t, before, liveFactCount(t, svc))
				return
			}
			require.False(t, r.IsError, "a learn_dedup: off topic must not be refused: %s", resultText(t, r))
			var parsed struct {
				Commits []struct{ File string } `json:"commits"`
			}
			require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &parsed))
			require.Len(t, parsed.Commits, 1)
			require.Equal(t, before+1, liveFactCount(t, svc))
		})
	}
}
