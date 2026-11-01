package mcp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/domain/modelparams"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// Characterization goldens for knomit_query's fused lens reads (queryFirstCall
// and queryRecent). They pin the tool's TEXT OUTPUT byte for byte for every
// case the fused search covers, so moving the fan-out, fusion and dedupe into
// one shared function cannot change a byte unnoticed. The REST twin is
// internal/edge/web/lens_fused_golden_test.go.
//
// Only per-run values are rewritten before the comparison: each mount's 12-hex
// repo id (to <id:name>), a full commit hash (to <sha>), each committed_at stamp
// (to its rank among the distinct stamps in the transcript, oldest = 1) and a
// cursor (to <cursor>).
//
// Regenerate with: go test ./internal/edge/mcp/ -run TestQueryFused -update-query-fused

var updateQueryFused = flag.Bool("update-query-fused", false, "rewrite testdata/query_fused/*.golden")

// fusedCosEmbedder makes relevance exact: a query embeds to e0, a document to
// (c, sqrt(1-c^2), 0...) where c is the `cos=<c>` marker in its body, so every
// document's cosine with every query is c.
type fusedCosEmbedder struct{}

var fusedCosMarker = regexp.MustCompile(`cos=([0-9.]+)`)

func (fusedCosEmbedder) doc(title, body string) []float32 {
	out := make([]float32, 768)
	m := fusedCosMarker.FindStringSubmatch(title + "\n" + body)
	if m == nil {
		out[1] = 1
		return out
	}
	c, _ := strconv.ParseFloat(m[1], 64)
	out[0] = float32(c)
	out[1] = float32(math.Sqrt(1 - c*c))
	return out
}

func (fusedCosEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	out := make([]float32, 768)
	out[0] = 1
	return out, nil
}

func (e fusedCosEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.doc(title, body), nil
}

func (e fusedCosEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.doc(titles[i], bodies[i])
	}
	return out, nil
}

func (e fusedCosEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		out[i] = e.doc(s, "")
	}
	return out, nil
}

func (fusedCosEmbedder) Dim() int                           { return 768 }
func (fusedCosEmbedder) ID() string                         { return "fused-cos" }
func (fusedCosEmbedder) Thresholds() modelparams.Thresholds { return modelparams.Defaults() }

// fusedRepo is a named code-ontology repo whose store embeds with
// fusedCosEmbedder.
func fusedRepo(t *testing.T, name string) *repos.RepoInstance {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(fusedCosEmbedder{})
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb", Embedder: fusedCosEmbedder{},
	})
}

type fusedSpec struct {
	path, title string
	cos         float64
	mod         func(*fact.Fact)
}

// writeFusedCommit writes every spec to ri's agent branch in ONE commit.
func writeFusedCommit(t *testing.T, ri *repos.RepoInstance, specs ...fusedSpec) {
	t.Helper()
	files := map[string]string{}
	for _, s := range specs {
		f := fact.NewFact(s.path)
		f.Title = s.title
		f.Body = "cos=" + strconv.FormatFloat(s.cos, 'f', -1, 64)
		f.Kind = fact.Epistemic
		f.Type = fact.Observation
		f.Confidence = 0.9
		f.Sources = 1
		if s.mod != nil {
			s.mod(&f)
		}
		text, err := fact.SerializeFact(f)
		require.NoError(t, err)
		files[s.path] = text
	}
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, _, err := svc.Facts().BatchWriteFacts(context.Background(), ri.AgentBranch(), files, nil, "add facts", "learn")
		require.NoError(t, err)
	}))
}

// nextSecond returns once the wall clock has moved to a later second.
func nextSecond() {
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(20 * time.Millisecond)
	}
}

func motifs(ms ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Motifs = ms } }

// fusedLens is the same three-mount corpus as the REST golden: alpha (mount 0),
// mid (mount 1, the WRITE repo), zulu (mount 2). kb/shared/1.md lives on alpha
// and mid (mid must win); alpha carries `config-drift` and zulu `drift-config`
// (only the lens's widening makes the term reach zulu); each mount is one
// commit in its own second.
func fusedLens(t *testing.T) (*repos.Binding, map[string]*repos.RepoInstance) {
	t.Helper()
	alpha, mid, zulu := fusedRepo(t, "alpha"), fusedRepo(t, "mid"), fusedRepo(t, "zulu")
	writeFusedCommit(t, alpha,
		fusedSpec{path: "kb/shared/1.md", title: "alpha shared copy", cos: 0.99},
		fusedSpec{path: "kb/a/1.md", title: "alpha one", cos: 0.8, mod: motifs("config-drift")},
		fusedSpec{path: "kb/a/2.md", title: "alpha two", cos: 0.6},
	)
	nextSecond()
	writeFusedCommit(t, mid,
		fusedSpec{path: "kb/shared/1.md", title: "mid shared copy", cos: 0.5},
		fusedSpec{path: "kb/m/1.md", title: "mid one", cos: 0.9, mod: func(f *fact.Fact) { f.Domain = []string{"store"} }},
	)
	nextSecond()
	writeFusedCommit(t, zulu,
		fusedSpec{path: "kb/z/1.md", title: "zulu one", cos: 0.95, mod: motifs("drift-config")},
		fusedSpec{path: "kb/z/2.md", title: "zulu two", cos: 0.7, mod: func(f *fact.Fact) { f.Kind = fact.Pragmatic; f.Type = fact.Policy }},
	)
	b := repos.NewBindingForTest(mid,
		repos.ReadTarget{RI: alpha, Branch: "agent/test"},
		repos.ReadTarget{RI: mid, Branch: "agent/test"},
		repos.ReadTarget{RI: zulu, Branch: "agent/test"},
	)
	return b, map[string]*repos.RepoInstance{"alpha": alpha, "mid": mid, "zulu": zulu}
}

var (
	fusedStampRe  = regexp.MustCompile(`"committed_at":(\d+)`)
	fusedSHARe    = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	fusedCursorRe = regexp.MustCompile(`"cursor":"[^"]*"`)
)

// fusedCall is one knomit_query call of a golden transcript. A non-empty
// cursorFrom names an earlier call (by its label) whose cursor this call resumes.
type fusedCall struct {
	label      string
	args       map[string]any
	cursorFrom string
}

// checkQueryFusedGolden runs the calls through QueryHandler(emb) and compares
// the normalized transcript with testdata/query_fused/<name>.golden.
func checkQueryFusedGolden(t *testing.T, b *repos.Binding, mounts map[string]*repos.RepoInstance, name string, calls ...fusedCall) {
	t.Helper()
	cursors := map[string]string{}
	var raw strings.Builder
	for _, c := range calls {
		args := map[string]any{}
		for k, v := range c.args {
			args[k] = v
		}
		if c.cursorFrom != "" {
			cur := cursors[c.cursorFrom]
			require.NotEmptyf(t, cur, "%s: call %q left no cursor", name, c.cursorFrom)
			args["cursor"] = cur
		}
		ctx := repos.WithBinding(context.Background(), b)
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		result, err := QueryHandler(fusedCosEmbedder{})(ctx, req)
		require.NoError(t, err)
		text := resultText(t, result)
		if m := regexp.MustCompile(`"cursor":"([^"]*)"`).FindStringSubmatch(text); m != nil {
			cursors[c.label] = m[1]
		}
		fmt.Fprintf(&raw, "== %s\nerror: %v\n%s\n", c.label, result.IsError, text)
	}
	got := raw.String()
	for n, ri := range mounts {
		got = strings.ReplaceAll(got, federate.ID12(ri.ID()), "<id:"+n+">")
	}
	got = fusedSHARe.ReplaceAllString(got, "<sha>")
	got = fusedCursorRe.ReplaceAllString(got, `"cursor":"<cursor>"`)
	var stamps []int64
	seen := map[int64]bool{}
	for _, m := range fusedStampRe.FindAllStringSubmatch(got, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if !seen[n] {
			seen[n] = true
			stamps = append(stamps, n)
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] < stamps[j] })
	rank := map[int64]int{}
	for i, s := range stamps {
		rank[s] = i + 1
	}
	got = fusedStampRe.ReplaceAllStringFunc(got, func(s string) string {
		n, _ := strconv.ParseInt(fusedStampRe.FindStringSubmatch(s)[1], 10, 64)
		return `"committed_at":"<t` + strconv.Itoa(rank[n]) + `>"`
	})

	path := filepath.Join("testdata", "query_fused", name+".golden")
	if *updateQueryFused {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read golden (regenerate with -update-query-fused)")
	if string(want) != got {
		t.Errorf("%s: tool output differs from %s\n--- want\n%s\n--- got\n%s", name, path, want, got)
	}
}

func TestQueryFused_Golden(t *testing.T) {
	b, mounts := fusedLens(t)
	checkQueryFusedGolden(t, b, mounts, "corpus",
		// relevance: semantic text, RRF over three mounts, write-first dedupe
		fusedCall{label: "relevance text", args: map[string]any{"text": "x"}},
		// relevance paged: the first page plus the snapshot remainder
		fusedCall{label: "relevance paged p1", args: map[string]any{"text": "x", "limit": 2}},
		fusedCall{label: "relevance paged p2", cursorFrom: "relevance paged p1", args: map[string]any{"limit": 2}},
		// max_results truncates after the dedupe
		fusedCall{label: "relevance max_results", args: map[string]any{"text": "x", "max_results": 3}},
		// filter-only (no text)
		fusedCall{label: "relevance filter-only", args: map[string]any{"type": []any{"observation"}}},
		// motif widening (zulu answers only through the union)
		fusedCall{label: "relevance motif", args: map[string]any{"text": "x", "motifs": []any{"config-drift"}}},
		// a path filter naming no mount
		fusedCall{label: "relevance unmounted path", args: map[string]any{"text": "x", "path": "kb://000000000000/kb/"}},
		// a kb://-qualified path restricts to one mount
		fusedCall{label: "relevance qualified path", args: map[string]any{"text": "x", "path": "kb://" + federate.ID12(mounts["zulu"].ID()) + "/kb/"}},
		// a min_similarity that removes every candidate: the notice
		fusedCall{label: "relevance cutoff notice", args: map[string]any{"text": "x", "min_similarity": 0.999}},
		// recency listing: committed_at merge, then RRF when text is present
		fusedCall{label: "recent", args: map[string]any{"sort": "recent"}},
		fusedCall{label: "recent text", args: map[string]any{"sort": "recent", "text": "x"}},
		fusedCall{label: "recent paged p1", args: map[string]any{"sort": "recent", "limit": 2}},
		fusedCall{label: "recent paged p2", cursorFrom: "recent paged p1", args: map[string]any{"limit": 2}},
		fusedCall{label: "recent max_results", args: map[string]any{"sort": "recent", "max_results": 2}},
		fusedCall{label: "recent motif", args: map[string]any{"sort": "recent", "motifs": []any{"config-drift"}}},
		fusedCall{label: "recent type", args: map[string]any{"sort": "recent", "type": []any{"policy"}}},
	)
}

func TestQueryFused_EmptyLensGolden(t *testing.T) {
	alpha, mid := fusedRepo(t, "alpha"), fusedRepo(t, "mid")
	b := repos.NewBindingForTest(mid,
		repos.ReadTarget{RI: alpha, Branch: "agent/test"},
		repos.ReadTarget{RI: mid, Branch: "agent/test"},
	)
	checkQueryFusedGolden(t, b, map[string]*repos.RepoInstance{"alpha": alpha, "mid": mid}, "empty",
		fusedCall{label: "relevance text", args: map[string]any{"text": "x"}},
		fusedCall{label: "relevance filter-only", args: map[string]any{"type": []any{"observation"}}},
		fusedCall{label: "recent", args: map[string]any{"sort": "recent"}},
		fusedCall{label: "recent text", args: map[string]any{"sort": "recent", "text": "x"}},
	)
}

func TestQueryFused_OneMemberLensGolden(t *testing.T) {
	solo := fusedRepo(t, "solo")
	writeFusedCommit(t, solo,
		fusedSpec{path: "kb/s/1.md", title: "solo one", cos: 0.7, mod: motifs("config-drift")},
		fusedSpec{path: "kb/s/2.md", title: "solo two", cos: 0.9, mod: motifs("drift-config")},
	)
	b := repos.NewBindingForTest(solo, repos.ReadTarget{RI: solo, Branch: "agent/test"})
	checkQueryFusedGolden(t, b, map[string]*repos.RepoInstance{"solo": solo}, "one_member",
		fusedCall{label: "relevance text", args: map[string]any{"text": "x"}},
		fusedCall{label: "relevance motif", args: map[string]any{"text": "x", "motifs": []any{"config-drift"}}},
		fusedCall{label: "recent", args: map[string]any{"sort": "recent"}},
		fusedCall{label: "recent text", args: map[string]any{"sort": "recent", "text": "x"}},
	)
}

// failSlowFused makes ri's named read wait d and then fail with err.
func failSlowFused(t *testing.T, ri *repos.RepoInstance, read index.ReadName, d time.Duration, err error) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		t.Cleanup(svc.Search().SetOverridesForTest(index.Overrides{Reads: func(got string) error {
			if got == string(read) {
				time.Sleep(d)
			}
			return nil
		}}))
		t.Cleanup(svc.Search().SetReadFaultForTest(func(got index.ReadName) error {
			if got == read {
				return err
			}
			return nil
		}))
	}))
}

// Two mounts fail; the LOWER-indexed one (mid, mount 1) finishes LAST, so a
// fan-out that reported the first failure to arrive would name zulu. The
// contract (b9c7ab96) is that the lowest index wins.
func TestQueryFused_MemberFailureGolden(t *testing.T) {
	b, mounts := fusedLens(t)
	failSlowFused(t, mounts["mid"], index.ReadSearch, 150*time.Millisecond, errors.New("mid search down"))
	failSlowFused(t, mounts["zulu"], index.ReadSearch, 0, errors.New("zulu search down"))
	checkQueryFusedGolden(t, b, mounts, "failure",
		fusedCall{label: "relevance text", args: map[string]any{"text": "x"}},
		fusedCall{label: "relevance filter-only", args: map[string]any{"type": []any{"observation"}}},
	)
}

func TestQueryFused_MemberFailureRecentGolden(t *testing.T) {
	b, mounts := fusedLens(t)
	failSlowFused(t, mounts["mid"], index.ReadRecentFacts, 150*time.Millisecond, errors.New("mid recent down"))
	failSlowFused(t, mounts["zulu"], index.ReadRecentFacts, 0, errors.New("zulu recent down"))
	checkQueryFusedGolden(t, b, mounts, "failure_recent",
		fusedCall{label: "recent", args: map[string]any{"sort": "recent"}},
		fusedCall{label: "recent text", args: map[string]any{"sort": "recent", "text": "x"}},
	)
}
