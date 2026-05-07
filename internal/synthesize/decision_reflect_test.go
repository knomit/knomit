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

// stubSearchIndex returns canned hits from Search, regardless of query.
// Used to drive the novelty check deterministically without spinning up an
// embedder. Other SearchIndex methods aren't called by ApplyReflectDecisions
// — accessing them panics, which is the contract we want.
type stubSearchIndex struct {
	store.SearchIndex
	hits []store.SearchResult
}

func (s *stubSearchIndex) Search(_ context.Context, _ string, _ store.SearchQuery) ([]store.SearchResult, error) {
	return s.hits, nil
}

const reflectTestThreshold = 0.85

// TestApplyReflect_AppliesReinforce — happy path: a single reinforce entry
// against a real methodology fact yields one row in
// methodology_reinforcements per (methodology, transition) pair, no fact
// files written.
func TestApplyReflect_AppliesReinforce(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()
	branch := sess.Branch

	const methPath = "kb/meta/reasoning/m.md"
	writeMethodologyForTest(t, svc, branch, methPath, "M", "Body of methodology M")

	result := ReflectResult{
		Reasoning: "h1 and h2 both fit m",
		Reinforce: []ReinforceEntry{{
			MethodologyPath: methPath,
			TransitionPaths: []string{"kb/h1.md", "kb/h2.md"},
			Rationale:       "same pattern in both",
		}},
	}

	err := ApplyReflectDecisions(ctx, svc.Facts(), &stubSearchIndex{}, svc.Methodology(),
		result, sess, "kb", reflectTestThreshold, nil)
	require.NoError(t, err)

	rows, err := svc.Methodology().ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "two transition_paths → two reinforcement rows")
	require.Equal(t, methPath, rows[0].MethodologyPath)
	require.Equal(t, "same pattern in both", rows[0].Rationale)
}

// TestApplyReflect_AppliesPropose — happy path: a single propose with a
// non-matching novelty search yields a methodology fact written to disk
// under the configured ontology root, type forced to "methodology"
// regardless of agent input.
func TestApplyReflect_AppliesPropose(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()
	branch := sess.Branch

	result := ReflectResult{
		Reasoning: "this lesson is new",
		Propose: []ProposeEntry{{
			Title:           "Verify embeddings before trusting them",
			Body:            "When ranking by similarity, confirm embeddings exist; tag-only fallback hides drift.",
			TopicPath:       "meta/reasoning",
			Confidence:      0.7,
			TransitionPaths: []string{"kb/h1.md"},
			NoveltyArgument: "no existing methodology covers embedding-fallback hygiene",
			Domain:          flexStrings{"meta", "reasoning", "methodology"},
		}},
	}

	err := ApplyReflectDecisions(ctx, svc.Facts(), &stubSearchIndex{}, svc.Methodology(),
		result, sess, "kb", reflectTestThreshold, nil)
	require.NoError(t, err)

	// Locate the written fact via search; assert it's of type=methodology.
	entries, _, err := svc.Search().RecentFacts(ctx, branch, "kb/meta/reasoning",
		"", 50, 0, []string{"methodology"}, nil, nil, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "propose must result in a methodology fact under kb/meta/reasoning")

	var found *store.RecentFactEntry
	for i, e := range entries {
		if e.Title == "Verify embeddings before trusting them" {
			found = &entries[i]
			break
		}
	}
	require.NotNil(t, found, "fact written by propose must appear in the index")
	require.Equal(t, "methodology", found.Type, "type must be forced to methodology, not whatever agent claimed")

	// No reinforcement row created — propose is a write-fact action, not a
	// reinforcement.
	rows, err := svc.Methodology().ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestApplyReflect_RejectsProposeTooSimilar — embedding-similarity gate:
// when novelty search returns a hit, the proposal is rejected with a
// structured error pointing at the conflicting path. No fact is written
// and no reinforcement row is created.
func TestApplyReflect_RejectsProposeTooSimilar(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()

	const conflictPath = "kb/meta/reasoning/existing.md"
	writeMethodologyForTest(t, svc, sess.Branch, conflictPath, "Existing", "Already on file")

	stub := &stubSearchIndex{hits: []store.SearchResult{{
		FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{Path: conflictPath, Title: "Existing", Type: "methodology"},
		},
		Score: 91.0, // 0.91 cosine — well above the 0.85 threshold
	}}}

	result := ReflectResult{
		Reasoning: "thought it was new",
		Propose: []ProposeEntry{{
			Title:           "Already on file (rephrased)",
			Body:            "Same lesson, different words",
			TopicPath:       "meta/reasoning",
			Confidence:      0.7,
			TransitionPaths: []string{"kb/h1.md"},
			NoveltyArgument: "thought no existing methodology fit",
		}},
	}

	err := ApplyReflectDecisions(ctx, svc.Facts(), stub, svc.Methodology(),
		result, sess, "kb", reflectTestThreshold, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), conflictPath, "rejection must name the conflicting methodology so the agent can reinforce it")
	require.Contains(t, strings.ToLower(err.Error()), "similar")

	// No methodology fact written under meta/reasoning beyond the seeded one.
	entries, _, err := svc.Search().RecentFacts(ctx, sess.Branch, "kb/meta/reasoning",
		"", 50, 0, []string{"methodology"}, nil, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the pre-seeded methodology should exist; rejected propose must not write")
}

// TestApplyReflect_RejectsReinforceUnknownPath — reinforce against a
// non-existent path is rejected before any rows are written.
func TestApplyReflect_RejectsReinforceUnknownPath(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()

	result := ReflectResult{
		Reinforce: []ReinforceEntry{{
			MethodologyPath: "kb/meta/reasoning/does-not-exist.md",
			TransitionPaths: []string{"kb/h1.md"},
			Rationale:       "x",
		}},
	}

	err := ApplyReflectDecisions(ctx, svc.Facts(), &stubSearchIndex{}, svc.Methodology(),
		result, sess, "kb", reflectTestThreshold, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does-not-exist.md")

	rows, err := svc.Methodology().ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, rows, "rejected reinforce must not insert any rows")
}

// TestApplyReflect_RejectsReinforceNonMethodologyTarget — the path
// resolves but the fact's type is not methodology. Reject; this is how
// callers learn the agent miscategorised something.
func TestApplyReflect_RejectsReinforceNonMethodologyTarget(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()

	const obsPath = "kb/meta/reasoning/observation.md"
	f := fact.NewFact(obsPath)
	f.Title = "Observation"
	f.Body = "Not a methodology"
	f.Type = fact.Observation
	f.Confidence = 0.7
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, sess.Branch, obsPath, body, "seed-observation", "")
	require.NoError(t, err)

	result := ReflectResult{
		Reinforce: []ReinforceEntry{{
			MethodologyPath: obsPath,
			TransitionPaths: []string{"kb/h1.md"},
			Rationale:       "x",
		}},
	}

	err = ApplyReflectDecisions(ctx, svc.Facts(), &stubSearchIndex{}, svc.Methodology(),
		result, sess, "kb", reflectTestThreshold, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "methodology")
	require.Contains(t, err.Error(), obsPath)
}

// TestApplyReflect_AcceptsAllEmpty — the legitimate "I looked, no lessons
// here" outcome: empty reinforce and empty propose produce no writes and
// no error.
func TestApplyReflect_AcceptsAllEmpty(t *testing.T) {
	svc, sess := newReflectTestEnv(t)
	ctx := context.Background()

	err := ApplyReflectDecisions(ctx, svc.Facts(), &stubSearchIndex{}, svc.Methodology(),
		ReflectResult{Reasoning: "no lessons today"}, sess, "kb", reflectTestThreshold, nil)
	require.NoError(t, err)

	rows, err := svc.Methodology().ListReinforcementsBySession(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// newReflectTestEnv stands up a fresh on-disk Service with one repo and a
// pipeline session ready for reflect-application tests.
func newReflectTestEnv(t *testing.T) (*store.Service, *store.PipelineSession) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	sess, err := svc.Pipeline().CreatePipelineSession(context.Background(), "review", "agent/test")
	require.NoError(t, err)
	return svc, sess
}

// writeMethodologyForTest seeds a methodology fact at the given path so
// reinforce-target validation has something to find.
func writeMethodologyForTest(t *testing.T, svc *store.Service, branch, path, title, body string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = fact.Methodology
	f.Confidence = 0.8
	f.Sources = 1
	f.Domain = []string{"meta", "reasoning", "methodology"}
	serialized, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, path, serialized, "seed-methodology", "")
	require.NoError(t, err)
}
