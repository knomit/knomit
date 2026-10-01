package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/embeddings/params"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// traceEmbedder is a deterministic stand-in for the ONNX embedder: a stable
// hash of the text, so identical texts embed identically (cosine 1 — a
// near-duplicate the review start's dedup pass merges) and unrelated texts
// land far apart.
type traceEmbedder struct{}

const traceVecDim = 768

func traceVec(text string) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum64()
	v := make([]float32, traceVecDim)
	var sum float64
	for i := range v {
		seed = seed*6364136223846793005 + 1442695040888963407
		v[i] = float32(int64(seed>>33)%2000-1000) / 1000
		sum += float64(v[i]) * float64(v[i])
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func (traceEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return traceVec(text), nil
}
func (traceEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return traceVec(title + " " + body), nil
}
func (traceEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = traceVec(titles[i] + " " + bodies[i])
	}
	return out, nil
}
func (traceEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = traceVec(t)
	}
	return out, nil
}
func (traceEmbedder) Dim() int                      { return traceVecDim }
func (traceEmbedder) ID() string                    { return "trace-stub" }
func (traceEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

// newTraceReviewE2E is newReviewE2E with an embedder, over the first round of
// seedTraceCorpus.
func newTraceReviewE2E(t *testing.T) *reviewE2E {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	dbPath := filepath.Join(t.TempDir(), "alpha.db")
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := traceEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	seedTraceCorpus(t, svc, "first")
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: "uid-alpha", Name: "alpha", State: repos.StateActive, Profile: "code", CreatedAt: 1,
	}))
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", Svc: svc, AgentBranch: "agent/test",
		Ontology: fact.CodeOntology(), OntologyRoot: "kb", Embedder: emb,
	}))
	cs := newClientSessionsStore(t)
	m.SetClientSessions(cs)
	s := &Server{Manager: m, ClientSessions: cs, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return &reviewE2E{t: t, h: s.NewAPIRouter(), svc: svc, dbPath: dbPath}
}

// seedTraceCorpus writes one round of review fodder: three duplicated pairs
// (identical title and body), unrelated to each other, plus three distinct
// facts. At the default cluster resolution (4.0) a lone similar pair is split
// into singletons, while three disjoint pairs are three communities of two —
// so a review start's planning pass merges each pair: six dedup commits.
func seedTraceCorpus(t *testing.T, svc *store.Service, round string) {
	t.Helper()
	write := func(slug, title, body string) {
		f := fact.NewFact("kb/architecture/test/" + round + "-" + slug + ".md")
		f.Title, f.Body, f.Type = title, body, fact.Observation
		f.Domain, f.Confidence, f.Sources = []string{"test"}, 0.5, 1
		content, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", f.Path(), content, "seed", "")
		require.NoError(t, err)
	}
	for _, pair := range []string{"red", "green", "blue"} {
		for _, n := range []string{"one", "two"} {
			write(pair+"-"+n, "A note about the "+round+" "+pair+" pipeline",
				"The "+round+" "+pair+" pipeline stores its state in one table")
		}
	}
	for _, slug := range []string{"beta", "gamma", "delta"} {
		write(slug, "Embedding model note "+round+" "+slug, "The embedding model is used for similarity search "+round+" "+slug)
	}
}

func (e *reviewE2E) head() string {
	e.t.Helper()
	h, err := e.svc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(e.t, err)
	return h
}

// T8, review half [N2]: a knomit_review START with a trace stamps the planning
// pass's dedup merges and removals (made on the start call's ctx), and every
// ANSWER with a trace stamps the commits its decision applies. A second
// round, started and answered WITHOUT a trace, stamps nothing at all (D-mint).
// Sabotage: drop applyTrace from ReviewHandler, or attach the trace to a ctx
// the reviewer does not receive (round one red); stamp a fixed set when
// absent (round two red).
func TestAgentTrace_E2E_ReviewStartAndAnswerStampTheirWrites(t *testing.T) {
	e := newTraceReviewE2E(t)
	sid, handle := e.client()
	want := "\n\nKnomit-Trace: task-review\nKnomit-Run: " + e2eRun + "\n"
	trace := map[string]any{"Knomit-Trace": "task-review", "Knomit-Run": e2eRun}
	check := func(traced bool, msg string) {
		t.Helper()
		if traced {
			require.True(t, strings.HasSuffix(msg, want), "%q", msg)
		} else {
			require.NotContains(t, msg, "Knomit-", "%q", msg)
		}
	}

	for _, traced := range []bool{true, false} {
		if !traced {
			seedTraceCorpus(t, e.svc, "second")
		}
		seeded := e.head()
		args := map[string]any{"binding": handle}
		if traced {
			args["trace"] = trace
		}
		turn, errText := e.review(sid, args)
		require.Empty(t, errText)
		require.NotNil(t, turn.Item, "fixture: the start leaves an item to answer")
		startCommits := commitsSince(t, e.svc, "agent/test", seeded)
		require.Len(t, startCommits, 6, "fixture: the start's dedup pass merges three pairs (a merge and a removal each)")
		for _, msg := range startCommits {
			require.True(t, strings.HasPrefix(msg, "dedup: "), "%q", msg)
			check(traced, msg)
		}

		answered := 0
		for i := 0; turn.Item != nil && !turn.Done; i++ {
			before := e.head()
			args := map[string]any{"binding": handle, "session_id": turn.SessionID, "item_id": turn.Item.ID,
				"response": answerFor(t, e, turn)}
			if traced {
				args["trace"] = trace
			}
			turn, errText = e.review(sid, args)
			require.Empty(t, errText)
			for _, msg := range commitsSince(t, e.svc, "agent/test", before) {
				answered++
				check(traced, msg)
			}
			require.Less(t, i, 20, "the session never finished")
		}
		require.Positive(t, answered, "fixture: an answer must write, or this proves nothing (traced=%v)", traced)
	}
}

// answerFor answers one review item so that it WRITES: a prune item raises the
// first member's confidence, a distill item synthesizes over its members.
func answerFor(t *testing.T, e *reviewE2E, turn reviewTurn) string {
	t.Helper()
	paths := itemPaths(t, e, turn)
	require.NotEmpty(t, paths, "item %d (%s) names no facts", turn.Item.ID, turn.Item.Type)
	switch turn.Item.Type {
	case "prune":
		return fmt.Sprintf(`{"decisions":[{"action":"keep","path":%q,"confidence":0.77,"reason":"corroborated"}]}`, paths[0])
	default:
		refs := `"` + strings.Join(paths, `","`) + `"`
		return fmt.Sprintf(`{"synthesize":[{"path":"kb/architecture/test/synth-%d.md","title":"Synthesis over item %d","body":"What the members share.","type":"synthesis","domain":["test"],"confidence":0.8,"entities":[],"refs":[%s]}],"retract":[]}`,
			turn.Item.ID, turn.Item.ID, refs)
	}
}

// itemPaths reads the fact paths of the item a turn serves.
func itemPaths(t *testing.T, e *reviewE2E, turn reviewTurn) []string {
	t.Helper()
	pending, err := e.svc.Pipeline().PendingPipelineWorkItems(context.Background(), turn.SessionID)
	require.NoError(t, err)
	var item *store.PipelineWorkItem
	for i := range pending {
		if pending[i].ID == turn.Item.ID {
			item = &pending[i]
		}
	}
	require.NotNil(t, item, "item %d is not pending", turn.Item.ID)
	// The item's payload names its members as quoted repo paths; this is a
	// test reading a fixture's JSON, not ref classification.
	var paths []string
	for _, m := range itemPathRE.FindAllStringSubmatch(item.FactsJSON, -1) {
		if !contains(paths, m[1]) {
			paths = append(paths, m[1])
		}
	}
	return paths
}

var itemPathRE = regexp.MustCompile(`"(kb/architecture/test/[^"]+)"`)

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
