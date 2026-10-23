package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// lensTopicsBody mirrors the unified tree-level wire shape.
type lensTopicsBody struct {
	Path     string `json:"path"`
	Children []struct {
		Name   string `json:"name"`
		IsDir  bool   `json:"is_dir"`
		Type   string `json:"type"`
		Title  string `json:"title"`
		Path   string `json:"path"`
		Source *struct {
			Repo string `json:"repo"`
			ID   string `json:"id"`
		} `json:"source"`
	} `json:"children"`
}

func decodeLensTopics(t *testing.T, rec *httptest.ResponseRecorder) lensTopicsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body lensTopicsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	return body
}

func (b lensTopicsBody) names() []string {
	out := make([]string, len(b.Children))
	for i, c := range b.Children {
		out[i] = c.Name
	}
	return out
}

// lensMountStores returns the live store of each named mount.
func lensMountStores(t *testing.T, m *repos.Manager, names ...string) map[string]*store.Service {
	t.Helper()
	out := map[string]*store.Service{}
	for _, n := range names {
		withMountStore(t, m, n, func(svc *store.Service) { out[n] = svc })
	}
	return out
}

// topicsLens is a lens "eng" with write mount alpha and read mount beta over
// real stores, plus a router. Facts are written by the caller.
func topicsLens(t *testing.T) (*repos.Manager, http.Handler) {
	t.Helper()
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m, OntologyRoot: "kb"}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	return m, r
}

// One tree level merged across mounts: same-named directories collapse into a
// single plain node (no source/path/type/title); fact leaves are kept
// per-mount, enriched from the OWNING mount's index, tagged with {repo,id},
// and addressed canonically (bare for the write mount, kb://<id12>/… for a
// read mount). Order: directories first (deduped, name-sorted), then leaves
// name-sorted.
func TestLensTopics_UnionMergedLevel(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"),
		plainFact("kb/gotchas/g.md", "G", nil),
		plainFact("kb/decisions/d1.md", "D1", nil),
		plainFact("kb/aaa.md", "Alpha fact", typed(fact.Principle)),
	)
	writeFactsInOneCommit(t, m.Get("beta"),
		plainFact("kb/decisions/d2.md", "D2", nil),
		plainFact("kb/bbb.md", "Beta fact", typed(fact.Hypothesis)),
	)
	seen := observeListDir(t, lensMountStores(t, m, "alpha", "beta"))

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	body := decodeLensTopics(t, rec)

	require.Equal(t, "kb", body.Path)
	// Both mounts listed the ROOT dirPath (the global ontology root), once.
	for _, repo := range []string{"alpha", "beta"} {
		require.Equal(t, []string{"kb"}, seen.of(repo), "ListDir paths of %s", repo)
	}
	require.Equal(t, []string{"decisions", "gotchas", "aaa.md", "bbb.md"}, body.names())
	// Dir nodes are plain: merged silently, no source, no wire path, no enrichment.
	for _, c := range body.Children[:2] {
		if !c.IsDir || c.Source != nil || c.Path != "" || c.Type != "" || c.Title != "" {
			t.Errorf("dir node %q must be plain {name,is_dir}, got %+v", c.Name, c)
		}
	}
	alphaRI, betaRI := m.Get("alpha"), m.Get("beta")
	// Write-mount leaf: BARE canonical path + alpha source + its own enrichment.
	aaa := body.Children[2]
	if aaa.IsDir || aaa.Path != "kb/aaa.md" || aaa.Type != "principle" || aaa.Title != "Alpha fact" {
		t.Errorf("write leaf: got %+v", aaa)
	}
	if aaa.Source == nil || aaa.Source.Repo != "alpha" || aaa.Source.ID != federate.ID12(alphaRI.ID()) {
		t.Errorf("write leaf source: got %+v", aaa.Source)
	}
	// Read-mount leaf: kb://<id12>/… canonical path + beta source, enriched
	// from beta's index (alpha has no kb/bbb.md, so the wrong mount yields none).
	bbb := body.Children[3]
	wantID := federate.ID12(betaRI.ID())
	if bbb.IsDir || bbb.Path != "kb://"+wantID+"/kb/bbb.md" || bbb.Type != "hypothesis" || bbb.Title != "Beta fact" {
		t.Errorf("read leaf: got %+v (want path kb://%s/kb/bbb.md)", bbb, wantID)
	}
	if bbb.Source == nil || bbb.Source.Repo != "beta" || bbb.Source.ID != wantID {
		t.Errorf("read leaf source: got %+v", bbb.Source)
	}
}

// A fact path present on BOTH mounts (a fork whose upstream is read-mounted
// shares the fork's fact UUIDs) dedups to a SINGLE leaf, the write mount's
// copy winning — so the tree agrees with the flat facts union (writeFirstWinners)
// instead of showing the shadowed read-mount copy the flat list hides. The two
// copies differ in title and type, so enrichment from the losing mount shows.
func TestLensTopics_SharedLeafDedupsWriteWins(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"),
		plainFact("kb/dup.md", "Alpha copy", typed(fact.Principle)),
	)
	writeFactsInOneCommit(t, m.Get("beta"),
		plainFact("kb/dup.md", "Beta copy", typed(fact.Insight)),
		plainFact("kb/beta-only.md", "Beta only", nil),
	)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics"))
	// dup.md appears ONCE (alpha wins); beta-only.md is the first leaf.
	require.Equal(t, []string{"beta-only.md", "dup.md"}, body.names())
	dup := body.Children[1]
	if dup.Source == nil || dup.Source.Repo != "alpha" || dup.Path != "kb/dup.md" ||
		dup.Title != "Alpha copy" || dup.Type != "principle" {
		t.Errorf("dup leaf must be the WRITE mount's copy (bare path, alpha source, alpha enrichment), got %+v", dup)
	}
	only := body.Children[0]
	if only.Source == nil || only.Source.Repo != "beta" || only.Title != "Beta only" {
		t.Errorf("beta-only leaf: got %+v", only)
	}
}

// The wildcard route drills one level: dirPath = ontologyRoot + "/" + nodePath
// on every mount, and the response path echoes it.
func TestLensTopics_NodePathListsSubdirectory(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/decisions/lens/a.md", "A", nil))
	writeFactsInOneCommit(t, m.Get("beta"), plainFact("kb/decisions/lens/b.md", "B", nil))
	seen := observeListDir(t, lensMountStores(t, m, "alpha", "beta"))

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics/decisions"))
	require.Equal(t, "kb/decisions", body.Path)
	for _, repo := range []string{"alpha", "beta"} {
		require.Equal(t, []string{"kb/decisions"}, seen.of(repo), "ListDir paths of %s", repo)
	}
	if len(body.Children) != 1 || body.Children[0].Name != "lens" || !body.Children[0].IsDir {
		t.Fatalf("children: got %+v, want the single merged dir \"lens\"", body.Children)
	}
}

// repo=<name> narrows the fan-out to the named mount(s): the other mounts are
// not listed AT ALL (narrowing happens before ListDir).
func TestLensTopics_RepoFilterNarrows(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/a.md", "A", nil))
	writeFactsInOneCommit(t, m.Get("beta"), plainFact("kb/b.md", "B", nil))
	seen := observeListDir(t, lensMountStores(t, m, "alpha", "beta"))

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics?repo=beta"))
	require.Equal(t, []string{"b.md"}, body.names(), "only beta's leaf")
	require.Empty(t, seen.of("alpha"), "alpha must not be listed when repo=beta narrows the fan-out")
	require.Equal(t, []string{"kb"}, seen.of("beta"))
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensTopics_UnknownRepoFilter422(t *testing.T) {
	_, r := topicsLens(t)

	rec := getLensFacts(t, r, "/lenses/eng/topics?repo=ghost")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// Drilling into kb/<topic>/… skips mounts whose ontology lacks the topic —
// the same decision-17 semantics as the facts/search/stats siblings. alpha is
// a "default"-preset repo (topics include `technology`); beta is a
// "code"-preset repo (invariants/architecture/… — NO `technology`), yet beta's
// store does hold a fact at that path, so only the skip keeps it out.
func TestLensTopics_TopicSkipUnderDeepPath(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	if _, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "beta", Mode: "preset", OntologyPreset: "code",
	}, nil); err != nil {
		t.Fatalf("create code-preset repo: %v", err)
	}
	r := (&Server{Manager: m, OntologyRoot: "kb"}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/technology/software/x.md", "X", nil))
	writeFactsInOneCommit(t, m.Get("beta"), plainFact("kb/technology/software/y.md", "Y", nil))
	stores := lensMountStores(t, m, "alpha", "beta")
	entries, err := stores["beta"].Facts().ListDir(t.Context(), m.Get("beta").AgentBranch(), "kb/technology/software")
	require.NoError(t, err)
	require.Len(t, entries, 1, "fixture: beta's store must hold a leaf the skip has to keep out")
	seen := observeListDir(t, stores)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics/technology/software"))
	require.Empty(t, seen.of("beta"), "beta (ontology lacks `technology`) must be SKIPPED under a deep path")
	require.Equal(t, []string{"kb/technology/software"}, seen.of("alpha"))
	require.Equal(t, []string{"x.md"}, body.names(), "only alpha's leaf")
}

// Any mount error fails the WHOLE request (RFC §9.1) — even when the failing
// mount is a read mount and the write repo answered fine.
func TestLensTopics_MountErrorFailsWholeRequest(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"), plainFact("kb/a.md", "A", nil))
	writeFactsInOneCommit(t, m.Get("beta"), plainFact("kb/b.md", "B", nil))
	injected := errors.New("git on fire")
	var hits atomic.Int64
	failListDir(t, lensMountStores(t, m, "beta")["beta"], "kb", injected, &hits)

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	require.Contains(t, rec.Body.String(), "Failed to list topics")
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load())
}

// An empty level serializes children as [] — never null.
func TestLensTopics_EmptyLevel(t *testing.T) {
	m, r := topicsLens(t)
	seen := observeListDir(t, lensMountStores(t, m, "alpha", "beta"))

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	body := decodeLensTopics(t, rec)
	for _, repo := range []string{"alpha", "beta"} {
		require.Equal(t, []string{"kb"}, seen.of(repo), "ListDir paths of %s", repo)
	}
	if len(body.Children) != 0 {
		t.Fatalf("children: got %+v, want none", body.Children)
	}
	if !strings.Contains(rec.Body.String(), `"children":[]`) {
		t.Errorf("children must serialize as [] not null; body=%s", rec.Body.String())
	}
}

// TestLensTopics_HidesPrivateDirectory pins spec/mbekg.md §3.8 for the lens
// union tree, the same rule the plain repo topicHandler enforces: a
// dot-prefixed directory is machinery, and the lens tree — knomit's own
// reader — must not surface it either, on any mount, dir or leaf alike.
func TestLensTopics_HidesPrivateDirectory(t *testing.T) {
	m, r := topicsLens(t)
	writeFactsInOneCommit(t, m.Get("alpha"),
		plainFact("kb/gotchas/g.md", "G", nil),
		plainFact("kb/.drafts/d.md", "Draft", nil),
	)
	writeFactsInOneCommit(t, m.Get("beta"),
		plainFact("kb/.hidden.md", "Hidden", nil),
		plainFact("kb/b.md", "B", nil),
	)
	stores := lensMountStores(t, m, "alpha", "beta")
	for name, want := range map[string]int{"alpha": 2, "beta": 2} {
		entries, err := stores[name].Facts().ListDir(t.Context(), m.Get(name).AgentBranch(), "kb")
		require.NoError(t, err)
		require.Len(t, entries, want, "fixture: %s's store must list its private entry for the guard to have work", name)
	}

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics"))
	require.Equal(t, []string{"gotchas", "b.md"}, body.names(), "private entries excluded")
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensTopics_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, OntologyRoot: "kb"}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/topics")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
