package synthesize

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/embeddings/params"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// restatementEmbedder is a deterministic stand-in for the ONNX embedder that
// counts what it was asked to do, so tests can assert on the SHAPE of the
// backfill (how many titles, through which rendering) rather than on vectors.
//
// vectorFor lets a test place facts at chosen positions on the axis; the
// default is a stable hash of the text, which gives unrelated strings low
// similarity and identical strings similarity 1.
type restatementEmbedder struct {
	titles           atomic.Int64
	shortStringCalls atomic.Int64
	documentCalls    atomic.Int64
	perBatchDelay    time.Duration
	vectorFor        func(text string) []float32
}

const testVecDim = 768

func (e *restatementEmbedder) vec(text string) []float32 {
	if e.vectorFor != nil {
		if v := e.vectorFor(text); v != nil {
			return v
		}
	}
	return hashVector(text)
}

func (e *restatementEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}

func (e *restatementEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	e.documentCalls.Add(1)
	return e.vec(title + " " + body), nil
}

func (e *restatementEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	e.documentCalls.Add(1)
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}

func (e *restatementEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	e.shortStringCalls.Add(1)
	e.titles.Add(int64(len(texts)))
	if e.perBatchDelay > 0 {
		time.Sleep(e.perBatchDelay)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vec(t)
	}
	return out, nil
}

func (e *restatementEmbedder) Dim() int                      { return testVecDim }
func (e *restatementEmbedder) ID() string                    { return "restatement-stub" }
func (e *restatementEmbedder) Thresholds() params.Thresholds { return params.Defaults() }

// hashVector maps text onto a stable unit vector. Distinct texts land far
// apart; identical texts land on top of each other.
func hashVector(text string) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum64()
	v := make([]float32, testVecDim)
	for i := range v {
		seed = seed*6364136223846793005 + 1442695040888963407
		v[i] = float32(int64(seed>>33)%2000-1000) / 1000
	}
	normalize(v)
	return v
}

// axisVector places a point at `angle` radians in the first two dimensions, so
// a test can dial the exact title-cosine between two facts.
func axisVector(angle float64) []float32 {
	v := make([]float32, testVecDim)
	v[0] = float32(math.Cos(angle))
	v[1] = float32(math.Sin(angle))
	return v
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum <= 1e-12 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// restatementEnv is a repo with n facts, wired the way the review pipeline
// wires itself, so tests can call the phase-0 functions directly with a real
// store behind them.
type restatementEnv struct {
	t      *testing.T
	svc    *store.Service
	ri     *repos.RepoInstance
	emb    *restatementEmbedder
	branch string
}

func newRestatementEnv(t *testing.T, n int) *restatementEnv {
	t.Helper()
	return newRestatementEnvWith(t, n, &restatementEmbedder{})
}

func newRestatementEnvWithoutEmbedder(t *testing.T, n int) *restatementEnv {
	t.Helper()
	return newRestatementEnvWith(t, n, nil)
}

func newRestatementEnvWith(t *testing.T, n int, emb *restatementEmbedder) *restatementEnv {
	t.Helper()
	branch := "agent/test"
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	if emb != nil {
		svc.SetEmbedder(emb)
	}
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	cfg := repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	}
	if emb != nil {
		cfg.Embedder = emb
	}
	env := &restatementEnv{t: t, svc: svc, ri: repos.NewTestInstanceWithDeps(cfg), emb: emb, branch: branch}

	for i := range n {
		env.writeFact(fmt.Sprintf("kb/f%d.md", i), fmt.Sprintf("F%d", i), fmt.Sprintf("body-%d", i))
	}
	return env
}

func (e *restatementEnv) writeFact(path, title, body string) {
	e.t.Helper()
	_, err := e.svc.Facts().WriteFact(context.Background(), e.branch, path,
		"---\ntype: observation\n---\n# "+title+"\n\n"+body, "write "+path, "test")
	require.NoError(e.t, err)
}

// deps mirrors Pipeline.deps for a test that drives the phase-0 functions
// directly rather than through a session.
func (e *restatementEnv) deps() Deps {
	return Deps{
		RI:          e.ri,
		Facts:       e.svc.Facts(),
		Search:      e.svc.Search(),
		Pipeline:    e.svc.Pipeline(),
		Branches:    e.svc.Branches(),
		Abstraction: e.svc.Abstraction(),
		Effort:      EffortNormal,
		OnProgress:  func(ProgressEvent) {},
	}
}

// factIDs returns the live fact ids keyed by path, for tests that need to talk
// about facts the way the axis does.
func (e *restatementEnv) factIDs() map[string]int64 {
	e.t.Helper()
	targets, err := e.svc.Abstraction().LiveFactsMissingTitleVector(context.Background(), e.branch, 10_000)
	require.NoError(e.t, err)
	out := make(map[string]int64, len(targets))
	for _, t := range targets {
		out[t.Path] = t.FactID
	}
	return out
}

// dedupThreshold is the active model's calibrated near-duplicate floor, the
// same value reviewStrategy.Plan hands to dedupCluster.
func (e *restatementEnv) dedupThreshold() float64 {
	return store.EmbedderThresholds(e.ri.Embedder()).Dedup
}

func containsPair(pairs []store.RestatementPair, a, b string) bool {
	for _, p := range pairs {
		if (p.APath == a && p.BPath == b) || (p.APath == b && p.BPath == a) {
			return true
		}
	}
	return false
}

// seedShortlist runs the backfill + refresh the way a review session does.
func (e *restatementEnv) seedShortlist() {
	e.t.Helper()
	ctx := context.Background()
	_, _, err := ensureTitleVectors(ctx, e.deps(), e.branch, titleBackfillBudget)
	require.NoError(e.t, err)
	require.NoError(e.t, refreshRestatementShortlist(ctx, e.deps(), e.branch, e.dedupThreshold()))
}

// recordVerdict stores a judge outcome for a pair, resolving the fact ids the
// way the apply path does.
func (e *restatementEnv) recordVerdict(aPath, bPath string, merged bool) {
	e.t.Helper()
	ctx := context.Background()
	ids := e.liveFactIDs()
	require.NoError(e.t, e.svc.Abstraction().RecordRestatementVerdict(ctx, e.branch, store.RestatementVerdict{
		APath: aPath, BPath: bPath,
		AFactID: ids[aPath], BFactID: ids[bPath],
		Merged: merged, JudgedAt: time.Now(),
	}))
}

// liveFactIDs maps live paths to their current fact ids.
func (e *restatementEnv) liveFactIDs() map[string]int64 {
	e.t.Helper()
	facts, err := e.svc.Abstraction().LiveEpistemicFacts(context.Background(), e.branch)
	require.NoError(e.t, err)
	out := make(map[string]int64, len(facts))
	for id, path := range facts {
		out[path] = id
	}
	return out
}

func (e *restatementEnv) titleOf(path string) string {
	e.t.Helper()
	f, err := e.svc.Search().GetByPath(context.Background(), e.branch, path)
	require.NoError(e.t, err)
	require.NotNil(e.t, f)
	return f.Title
}

func (e *restatementEnv) writeFactWithDomain(path, title, body, domain string) {
	e.t.Helper()
	_, err := e.svc.Facts().WriteFact(context.Background(), e.branch, path,
		"---\ntype: observation\ndomain: ["+domain+"]\n---\n# "+title+"\n\n"+body,
		"write "+path, "test")
	require.NoError(e.t, err)
}

// keepVerdicts is n judge outcomes that all said "keep".
func keepVerdicts(n int) []store.RestatementVerdict {
	out := make([]store.RestatementVerdict, 0, n)
	for i := range n {
		out = append(out, store.RestatementVerdict{
			APath: fmt.Sprintf("kb/a%d.md", i), BPath: fmt.Sprintf("kb/b%d.md", i),
			AFactID: int64(i * 2), BFactID: int64(i*2 + 1),
		})
	}
	return out
}

// newRestatementEnvOnAxis builds a corpus whose TITLES sit at a controlled
// spacing on the abstraction axis, so a test can compare what two differently
// shaped corpora do under identical code. spacing is the angular gap in
// radians between consecutive titles: small spacing means a corpus whose titles
// all look alike.
func newRestatementEnvOnAxis(t *testing.T, n int, spacing float64) *restatementEnv {
	t.Helper()
	titleAngles := map[string]float64{}
	emb := &restatementEmbedder{}
	emb.vectorFor = func(text string) []float32 {
		angle, ok := titleAngles[text]
		if !ok {
			return nil // bodies keep the default hash placement
		}
		return axisVector(angle)
	}
	env := newRestatementEnvWith(t, 0, emb)
	for i := range n {
		title := fmt.Sprintf("Title %d", i)
		titleAngles[title] = float64(i) * spacing
		env.writeFact(fmt.Sprintf("kb/f%d.md", i), title, fmt.Sprintf("body %d with its own distinct wording", i))
	}
	return env
}

// workItems drains the session's queue for inspection.
func (e *restatementEnv) workItems(sessionID string) []store.PipelineWorkItem {
	e.t.Helper()
	var out []store.PipelineWorkItem
	seen := map[int64]bool{}
	for range 5000 {
		item, err := e.svc.Pipeline().NextPipelineWorkItem(context.Background(), sessionID)
		require.NoError(e.t, err)
		if item == nil || seen[item.ID] {
			break
		}
		seen[item.ID] = true
		out = append(out, *item)
		_, err = e.svc.Pipeline().AnswerPipelineWorkItem(context.Background(), item.ID, "{}")
		require.NoError(e.t, err)
	}
	return out
}
