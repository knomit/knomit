package synthtest

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/vocab"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/domain/modelparams"
	"knomit/internal/engine/store"
)

// RestatementEmbedder is a deterministic stand-in for the ONNX embedder that
// counts what it was asked to do, so tests can assert on the SHAPE of the
// backfill (how many titles, through which rendering) rather than on vectors.
//
// vectorFor lets a test place facts at chosen positions on the axis; the
// default is a stable hash of the text, which gives unrelated strings low
// similarity and identical strings similarity 1.
type RestatementEmbedder struct {
	Titles           atomic.Int64
	ShortStringCalls atomic.Int64
	DocumentCalls    atomic.Int64
	PerBatchDelay    time.Duration
	VectorFor        func(text string) []float32
}

const testVecDim = 768

func (e *RestatementEmbedder) vec(text string) []float32 {
	if e.VectorFor != nil {
		if v := e.VectorFor(text); v != nil {
			return v
		}
	}
	return HashVector(text)
}

func (e *RestatementEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vec(text), nil
}

func (e *RestatementEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	e.DocumentCalls.Add(1)
	return e.vec(title + " " + body), nil
}

func (e *RestatementEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	e.DocumentCalls.Add(1)
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}

func (e *RestatementEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	e.ShortStringCalls.Add(1)
	e.Titles.Add(int64(len(texts)))
	if e.PerBatchDelay > 0 {
		time.Sleep(e.PerBatchDelay)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vec(t)
	}
	return out, nil
}

func (e *RestatementEmbedder) Dim() int                           { return testVecDim }
func (e *RestatementEmbedder) ID() string                         { return "restatement-stub" }
func (e *RestatementEmbedder) Thresholds() modelparams.Thresholds { return modelparams.Defaults() }

// HashVector maps text onto a stable unit vector. Distinct texts land far
// apart; identical texts land on top of each other.
func HashVector(text string) []float32 {
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

// AxisVector places a point at `angle` radians in the first two dimensions, so
// a test can dial the exact title-cosine between two facts.
func AxisVector(angle float64) []float32 {
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

// RestatementEnv is a repo with n facts, wired the way the review pipeline
// wires itself, so tests can call the phase-0 functions directly with a real
// store behind them.
type RestatementEnv struct {
	T      *testing.T
	Svc    *store.Service
	RI     *repos.RepoInstance
	Emb    *RestatementEmbedder
	Branch string
}

func NewRestatementEnv(t *testing.T, n int) *RestatementEnv {
	t.Helper()
	return NewRestatementEnvWith(t, n, &RestatementEmbedder{})
}

func NewRestatementEnvWithoutEmbedder(t *testing.T, n int) *RestatementEnv {
	t.Helper()
	return NewRestatementEnvWith(t, n, nil)
}

func NewRestatementEnvWith(t *testing.T, n int, emb *RestatementEmbedder) *RestatementEnv {
	t.Helper()
	branch := "agent/test"
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	if emb != nil {
		svc.SetEmbedder(emb)
	}
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))

	cfg := repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
		// The production discovery.quality defaults (config.Defaults()). A bare
		// test instance leaves these zero, and a zero MaxMembers gates out
		// EVERY bridge candidate on every axis — so a discovery test on a bare
		// instance measures an engine that cannot emit anything, and passes.
		Quality: &repos.TestQualityConfig{
			CohFloor: 0.5, QualityFloor: 0.0, WCoh: 1.0, WGap: 1.0, WSpec: 1.0, MaxMembers: 5,
		},
	}
	if emb != nil {
		cfg.Embedder = emb
	}
	env := &RestatementEnv{T: t, Svc: svc, RI: repos.NewTestInstanceWithDeps(cfg), Emb: emb, Branch: branch}

	for i := range n {
		env.WriteFact(fmt.Sprintf("kb/f%d.md", i), fmt.Sprintf("F%d", i), fmt.Sprintf("body-%d", i))
	}
	return env
}

func (e *RestatementEnv) WriteFact(path, title, body string) {
	e.T.Helper()
	_, err := e.Svc.Facts().WriteFact(context.Background(), e.Branch, path,
		"---\ntype: observation\n---\n# "+title+"\n\n"+body, "write "+path, "test")
	require.NoError(e.T, err)
}

// WriteFiller writes the standard filler corpus — n facts with a common year
// token and a unique widget number each, the same paths, titles and bodies the
// TestStructural_* fixtures always wrote — in ONE BatchWriteFacts commit rather
// than one WriteFact commit per fact. Over 404 facts that is the difference
// between 404 commits and index syncs and one, which was most of
// internal/corpus/synthesize's run time.
//
// Every filler fact is minted before anything the caller writes next, exactly
// as before, so a fixture that writes its pair or twins AFTER the filler (as
// every caller does) keeps their fact ids above the filler and in their own
// write order. Within the filler the ids follow the batch's indexing order
// rather than i; nothing a test reads depends on that (the filler is
// interchangeable by construction), and the fixtures' standing shortlist,
// selection and health were compared before and after this change and matched.
func (e *RestatementEnv) WriteFiller(n int) {
	e.T.Helper()
	files := make(map[string]string, n)
	for i := range n {
		files[fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1)] =
			"---\ntype: observation\n---\n# " + fmt.Sprintf("Filler note 2026 about widget %d", 1000+i) + "\n\nan unrelated body"
	}
	_, _, err := e.Svc.Facts().BatchWriteFacts(context.Background(), e.Branch, files, nil, "write filler", "test")
	require.NoError(e.T, err)
}

// Deps mirrors Pipeline.Deps for a test that drives the phase-0 functions
// directly rather than through a session.
func (e *RestatementEnv) Deps() step.Deps {
	return step.Deps{
		RI:          e.RI,
		Facts:       e.Svc.Facts(),
		Files:       e.Svc.FactsRepo(),
		Search:      e.Svc.Search(),
		Pipeline:    e.Svc.Pipeline(),
		HeadCommit:  e.Svc.HeadCommit,
		Abstraction: e.Svc.Abstraction(),
		Motifs:      e.Svc.Motifs(),
		Effort:      step.EffortNormal,
		OnProgress:  func(step.ProgressEvent) {},
	}
}

// FactIDs returns the live fact ids keyed by path, for tests that need to talk
// about facts the way the axis does.
func (e *RestatementEnv) FactIDs() map[string]int64 {
	e.T.Helper()
	targets, err := e.Svc.Abstraction().LiveFactsMissingTitleVector(context.Background(), e.Branch, 10_000)
	require.NoError(e.T, err)
	out := make(map[string]int64, len(targets))
	for _, t := range targets {
		out[t.Path] = t.FactID
	}
	return out
}

// DedupThreshold is the active model's calibrated near-duplicate floor, the
// same value reviewStrategy.Plan hands to dedupCluster.
func (e *RestatementEnv) DedupThreshold() float64 {
	return model.EmbedderThresholds(e.RI.Embedder()).Dedup
}

func ContainsPair(pairs []vocab.RestatementPair, a, b string) bool {
	for _, p := range pairs {
		if (p.APath == a && p.BPath == b) || (p.APath == b && p.BPath == a) {
			return true
		}
	}
	return false
}

// RecordVerdict stores a judge outcome for a pair, resolving the fact ids the
// way the apply path does, and retires the pair the way the apply path does.
func (e *RestatementEnv) RecordVerdict(aPath, bPath string, merged bool) {
	e.T.Helper()
	ctx := context.Background()
	ids := e.LiveFactIDs()
	require.NoError(e.T, e.Svc.Abstraction().RecordRestatementVerdict(ctx, e.Branch, vocab.RestatementVerdict{
		APath: aPath, BPath: bPath,
		AFactID: ids[aPath], BFactID: ids[bPath],
		Resolved: merged, JudgedAt: time.Now(),
	}))
	require.NoError(e.T, e.Svc.Abstraction().DeleteRestatementPair(ctx, e.Branch, ids[aPath], ids[bPath]))
}

// LiveFactIDs maps live paths to their current fact ids.
func (e *RestatementEnv) LiveFactIDs() map[string]int64 {
	e.T.Helper()
	facts, err := e.Svc.Abstraction().LiveEpistemicFacts(context.Background(), e.Branch)
	require.NoError(e.T, err)
	out := make(map[string]int64, len(facts))
	for id, path := range facts {
		out[path] = id
	}
	return out
}

func (e *RestatementEnv) TitleOf(path string) string {
	e.T.Helper()
	f, err := e.Svc.Search().GetByPath(context.Background(), e.Branch, path)
	require.NoError(e.T, err)
	require.NotNil(e.T, f)
	return f.Title
}

func (e *RestatementEnv) WriteFactWithDomain(path, title, body, domain string) {
	e.T.Helper()
	_, err := e.Svc.Facts().WriteFact(context.Background(), e.Branch, path,
		"---\ntype: observation\ndomain: ["+domain+"]\n---\n# "+title+"\n\n"+body,
		"write "+path, "test")
	require.NoError(e.T, err)
}

// WriteFactWithMotifs mirrors writeFact but carries motifs, so the motif
// dynamics tests can drive whole sessions over a corpus that has them.
//
// It renders through SerializeFact rather than hand-building frontmatter like
// its siblings above: motifs are the one field with a WRITE-TIME transform
// (the silent subject strip), so a hand-built fixture would be a string no
// real writer could produce.
func (e *RestatementEnv) WriteFactWithMotifs(path, title, body string, motifs []string) {
	e.T.Helper()
	e.WriteFactWithMotifsConf(path, title, body, motifs, 0.7)
}

// WriteFactWithMotifsConf is writeFactWithMotifs with an explicit confidence,
// for tests that need to decide which of two facts wins a merge.
func (e *RestatementEnv) WriteFactWithMotifsConf(path, title, body string, motifs []string, confidence float64) {
	e.T.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = fact.Observation
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = confidence
	f.Sources = 1
	f.Motifs = motifs
	rendered, err := fact.SerializeFact(f)
	require.NoError(e.T, err)
	_, err = e.Svc.Facts().WriteFact(context.Background(), e.Branch, f.Path(),
		rendered, "write "+path, "test")
	require.NoError(e.T, err)
}

// KeepVerdicts is n judge outcomes that all said "keep".
func KeepVerdicts(n int) []vocab.RestatementVerdict {
	out := make([]vocab.RestatementVerdict, 0, n)
	for i := range n {
		out = append(out, vocab.RestatementVerdict{
			APath: fmt.Sprintf("kb/a%d.md", i), BPath: fmt.Sprintf("kb/b%d.md", i),
			AFactID: int64(i * 2), BFactID: int64(i*2 + 1),
		})
	}
	return out
}

// NewRestatementEnvOnAxis builds a corpus whose TITLES sit at a controlled
// spacing on the abstraction axis, so a test can compare what two differently
// shaped corpora do under identical code. spacing is the angular gap in
// radians between consecutive titles: small spacing means a corpus whose titles
// all look alike.
func NewRestatementEnvOnAxis(t *testing.T, n int, spacing float64) *RestatementEnv {
	t.Helper()
	titleAngles := map[string]float64{}
	emb := &RestatementEmbedder{}
	emb.VectorFor = func(text string) []float32 {
		angle, ok := titleAngles[text]
		if !ok {
			return nil // bodies keep the default hash placement
		}
		return AxisVector(angle)
	}
	env := NewRestatementEnvWith(t, 0, emb)
	for i := range n {
		title := fmt.Sprintf("Title %d", i)
		titleAngles[title] = float64(i) * spacing
		env.WriteFact(fmt.Sprintf("kb/f%d.md", i), title, fmt.Sprintf("body %d with its own distinct wording", i))
	}
	return env
}

// WorkItems drains the session's queue for inspection.
func (e *RestatementEnv) WorkItems(sessionID string) []model.PipelineWorkItem {
	e.T.Helper()
	var out []model.PipelineWorkItem
	seen := map[int64]bool{}
	for range 5000 {
		item, err := e.Svc.Pipeline().NextPipelineWorkItem(context.Background(), sessionID)
		require.NoError(e.T, err)
		if item == nil || seen[item.ID] {
			break
		}
		seen[item.ID] = true
		out = append(out, *item)
		_, err = e.Svc.Pipeline().AnswerPipelineWorkItem(context.Background(), item.ID, "{}")
		require.NoError(e.T, err)
	}
	return out
}

// FactsJSON renders the two named facts the way a prune work item carries them.
func (e *RestatementEnv) FactsJSON(paths ...string) string {
	e.T.Helper()
	facts := make([]step.FactForLLM, 0, len(paths))
	for _, p := range paths {
		f, err := e.Svc.Search().GetByPath(context.Background(), e.Branch, p)
		require.NoError(e.T, err)
		require.NotNil(e.T, f)
		facts = append(facts, step.FactForLLM{File: f.Path, Title: f.Title, Body: f.Body, Type: f.Type})
	}
	out, err := json.Marshal(facts)
	require.NoError(e.T, err)
	return string(out)
}

// ManyTwinsEnvWithFiller builds a corpus with n path-identity twin pairs, with the filler size chosen by the
// caller.
//
// The filler exists for ONE reason — both rarity cuts are read off the corpus's
// own token distribution, so a corpus with no distribution has nothing rare in
// it — and the standard 404 is sized for `shortlistBudget`, which is a
// SELECTION concern. Tests about the standing channel do not need it: the
// allowance fires whatever the budget is, and 404 facts make the distill item
// span six pages, which turns an unrelated paging contract into a fixture
// problem. So they pass a smaller number and assert the detection still fires.
func ManyTwinsEnvWithFiller(t *testing.T, n, filler int) *RestatementEnv {
	t.Helper()
	// Subject tokens chosen to be rare in this corpus and unrelated to each
	// other; the prefix-EXTENSION shape (subject vs vendor/subject) is the one
	// measured on the live corpus.
	subjects := []string{
		"cisco", "gitlab", "geoserver", "sharepoint", "ivanti",
		"sonicwall", "fortinet", "confluence", "jenkins", "grafana",
	}
	require.LessOrEqual(t, n, len(subjects), "ManyTwinsEnv has only %d subjects", len(subjects))

	env := NewRestatementEnv(t, 0)
	env.WriteFiller(filler)
	for i, s := range subjects[:n] {
		env.WriteFact(
			fmt.Sprintf("kb/technology/security/vulnerabilities/%s/%08x.md", s, 0xAA0000+i*2),
			fmt.Sprintf("%s advisory covers a remote code execution flaw", s),
			"a body about the event")
		env.WriteFact(
			fmt.Sprintf("kb/technology/security/vulnerabilities/vendor/%s/%08x.md", s, 0xAA0001+i*2),
			fmt.Sprintf("Remote code execution flaw disclosed in %s", s),
			"the same event, filed again")
	}
	return env
}

// UnitHash maps text to a stable value in [0,1). Used to place motif names at
// distinct, deterministic angles: a fixture that derives position from a
// property several names share (length, a small modulus) puts them on top of
// each other and silently turns a distribution test into a constant test.
func UnitHash(text string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return float64(h.Sum64()%1_000_000) / 1_000_000
}

// NumberWord keeps generated motif names to the 2–4 word kebab shape the field
// contract requires — "mechanism-17" would be rejected as a digit token.
func NumberWord(i int) string {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
		"golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november",
		"oscar", "papa", "quebec", "romeo", "sierra", "tango"}
	return words[i%len(words)]
}
