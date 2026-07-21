package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// lensTopicsStub is a per-repo TopicLister: each mount's ListDir/GetByPath is
// answered from maps keyed by the repo's name, so one stub drives the whole
// fan-out. It records the last dirPath ListDir saw per repo (which doubles as
// a "was this mount fanned out to at all" probe for the narrowing/skip tests);
// errRepo makes that one mount's ListDir fail.
type lensTopicsStub struct {
	dirsByRepo  map[string][]store.DirEntry
	factsByRepo map[string]map[string]*store.FactWithBody // repo → fullPath → fact
	errRepo     string
	listPaths   map[string]string
}

func (s *lensTopicsStub) ListDir(_ context.Context, ri *repos.RepoInstance, _ string, path string) ([]store.DirEntry, error) {
	if s.listPaths == nil {
		s.listPaths = map[string]string{}
	}
	s.listPaths[ri.Name()] = path
	if s.errRepo != "" && ri.Name() == s.errRepo {
		return nil, errors.New("git on fire")
	}
	return s.dirsByRepo[ri.Name()], nil
}

func (s *lensTopicsStub) GetByPath(_ context.Context, ri *repos.RepoInstance, _ string, path string) (*store.FactWithBody, error) {
	if m := s.factsByRepo[ri.Name()]; m != nil {
		if fb, ok := m[path]; ok {
			return fb, nil
		}
	}
	return nil, nil
}

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

// One tree level merged across mounts: same-named directories collapse into a
// single plain node (no source/path/type/title); fact leaves are kept
// per-mount, enriched via GetByPath, tagged with {repo,id}, and addressed
// canonically (bare for the write mount, kb://<id12>/… for a read mount).
// Order: directories first (deduped, name-sorted), then leaves name-sorted.
func TestLensTopics_UnionMergedLevel(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			"alpha": {{Name: "gotchas", IsDir: true}, {Name: "decisions", IsDir: true}, {Name: "aaa.md", IsDir: false}},
			"beta":  {{Name: "decisions", IsDir: true}, {Name: "bbb.md", IsDir: false}},
		},
		factsByRepo: map[string]map[string]*store.FactWithBody{
			"alpha": {"kb/aaa.md": {FactRecord: store.FactRecord{Title: "Alpha fact", Type: "decision"}}},
			"beta":  {"kb/bbb.md": {FactRecord: store.FactRecord{Title: "Beta fact", Type: "gotcha"}}},
		},
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	body := decodeLensTopics(t, rec)

	if body.Path != "kb" {
		t.Errorf("path: got %q, want kb", body.Path)
	}
	// Both mounts listed the ROOT dirPath (the global ontology root).
	for _, repo := range []string{"alpha", "beta"} {
		if got := stub.listPaths[repo]; got != "kb" {
			t.Errorf("ListDir path for %s: got %q, want kb", repo, got)
		}
	}
	names := make([]string, len(body.Children))
	for i, c := range body.Children {
		names[i] = c.Name
	}
	want := []string{"decisions", "gotchas", "aaa.md", "bbb.md"}
	if len(names) != len(want) {
		t.Fatalf("children: got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("children order: got %v, want %v", names, want)
		}
	}
	// Dir nodes are plain: merged silently, no source, no wire path, no enrichment.
	for _, c := range body.Children[:2] {
		if !c.IsDir || c.Source != nil || c.Path != "" || c.Type != "" || c.Title != "" {
			t.Errorf("dir node %q must be plain {name,is_dir}, got %+v", c.Name, c)
		}
	}
	alphaRI, betaRI := m.Get("alpha"), m.Get("beta")
	// Write-mount leaf: BARE canonical path + alpha source + per-mount enrichment.
	aaa := body.Children[2]
	if aaa.IsDir || aaa.Path != "kb/aaa.md" || aaa.Type != "decision" || aaa.Title != "Alpha fact" {
		t.Errorf("write leaf: got %+v", aaa)
	}
	if aaa.Source == nil || aaa.Source.Repo != "alpha" || aaa.Source.ID != federate.ID12(alphaRI.ID()) {
		t.Errorf("write leaf source: got %+v", aaa.Source)
	}
	// Read-mount leaf: kb://<id12>/… canonical path + beta source.
	bbb := body.Children[3]
	wantID := federate.ID12(betaRI.ID())
	if bbb.IsDir || bbb.Path != "kb://"+wantID+"/kb/bbb.md" || bbb.Type != "gotcha" || bbb.Title != "Beta fact" {
		t.Errorf("read leaf: got %+v (want path kb://%s/kb/bbb.md)", bbb, wantID)
	}
	if bbb.Source == nil || bbb.Source.Repo != "beta" || bbb.Source.ID != wantID {
		t.Errorf("read leaf source: got %+v", bbb.Source)
	}
}

// A fact path present on BOTH mounts (a fork whose upstream is read-mounted
// shares the fork's fact UUIDs) dedups to a SINGLE leaf, the write mount's
// copy winning — so the tree agrees with the flat facts union (writeFirstWinners)
// instead of showing the shadowed read-mount copy the flat list hides.
func TestLensTopics_SharedLeafDedupsWriteWins(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			// beta (read) is fanned out first in binding order, but the write
			// mount alpha must still win the shared "dup.md".
			"alpha": {{Name: "dup.md", IsDir: false}},
			"beta":  {{Name: "dup.md", IsDir: false}, {Name: "beta-only.md", IsDir: false}},
		},
		factsByRepo: map[string]map[string]*store.FactWithBody{
			"alpha": {"kb/dup.md": {FactRecord: store.FactRecord{Title: "Alpha copy", Type: "decision"}}},
			"beta":  {"kb/dup.md": {FactRecord: store.FactRecord{Title: "Beta copy", Type: "gotcha"}}},
		},
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics"))
	// dup.md appears ONCE (alpha wins); beta-only.md is the second leaf.
	names := []string{}
	for _, c := range body.Children {
		names = append(names, c.Name)
	}
	if len(names) != 2 || names[0] != "beta-only.md" || names[1] != "dup.md" {
		t.Fatalf("children: got %v, want [beta-only.md dup.md] (dup deduped)", names)
	}
	dup := body.Children[1]
	if dup.Source == nil || dup.Source.Repo != "alpha" || dup.Path != "kb/dup.md" || dup.Title != "Alpha copy" {
		t.Errorf("dup leaf must be the WRITE mount's copy (bare path, alpha source), got %+v", dup)
	}
}

// The wildcard route drills one level: dirPath = ontologyRoot + "/" + nodePath
// on every mount, and the response path echoes it.
func TestLensTopics_NodePathListsSubdirectory(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			"alpha": {{Name: "lens", IsDir: true}},
			"beta":  {{Name: "lens", IsDir: true}},
		},
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics/decisions"))
	if body.Path != "kb/decisions" {
		t.Errorf("path: got %q, want kb/decisions", body.Path)
	}
	for _, repo := range []string{"alpha", "beta"} {
		if got := stub.listPaths[repo]; got != "kb/decisions" {
			t.Errorf("ListDir path for %s: got %q, want kb/decisions", repo, got)
		}
	}
	if len(body.Children) != 1 || body.Children[0].Name != "lens" || !body.Children[0].IsDir {
		t.Fatalf("children: got %+v, want the single merged dir \"lens\"", body.Children)
	}
}

// repo=<name> narrows the fan-out to the named mount(s): the other mounts are
// not listed AT ALL (narrowing happens before ListDir).
func TestLensTopics_RepoFilterNarrows(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			"alpha": {{Name: "a.md", IsDir: false}},
			"beta":  {{Name: "b.md", IsDir: false}},
		},
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics?repo=beta"))
	if len(body.Children) != 1 || body.Children[0].Name != "b.md" {
		t.Fatalf("children: got %+v, want only beta's leaf", body.Children)
	}
	if _, called := stub.listPaths["alpha"]; called {
		t.Error("alpha must not be listed when repo=beta narrows the fan-out")
	}
}

// An unknown repo= name is a well-formed request naming a nonexistent mount → 422.
func TestLensTopics_UnknownRepoFilter422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: &lensTopicsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

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
// "code"-preset repo (invariants/architecture/… — NO `technology`).
func TestLensTopics_TopicSkipUnderDeepPath(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	if _, err := m.Create(context.Background(), repos.CreateSpec{
		Name: "beta", Mode: "preset", OntologyPreset: "code",
	}, nil); err != nil {
		t.Fatalf("create code-preset repo: %v", err)
	}
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			"alpha": {{Name: "x.md", IsDir: false}},
			"beta":  {{Name: "y.md", IsDir: false}},
		},
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	body := decodeLensTopics(t, getLensFacts(t, r, "/lenses/eng/topics/technology/software"))
	if _, called := stub.listPaths["beta"]; called {
		t.Error("beta (ontology lacks `technology`) must be SKIPPED under a deep path")
	}
	if got := stub.listPaths["alpha"]; got != "kb/technology/software" {
		t.Errorf("alpha ListDir path: got %q, want kb/technology/software", got)
	}
	if len(body.Children) != 1 || body.Children[0].Name != "x.md" {
		t.Fatalf("children: got %+v, want only alpha's leaf", body.Children)
	}
}

// Any mount error fails the WHOLE request (RFC §9.1) — even when the failing
// mount is a read mount and the write repo answered fine.
func TestLensTopics_MountErrorFailsWholeRequest(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	stub := &lensTopicsStub{
		dirsByRepo: map[string][]store.DirEntry{
			"alpha": {{Name: "a.md", IsDir: false}},
		},
		errRepo: "beta",
	}
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: stub}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// An empty level serializes children as [] — never null.
func TestLensTopics_EmptyLevel(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: &lensTopicsStub{}}}
	r := s.NewAPIRouter()
	createLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)

	rec := getLensFacts(t, r, "/lenses/eng/topics")
	body := decodeLensTopics(t, rec)
	if len(body.Children) != 0 {
		t.Fatalf("children: got %+v, want none", body.Children)
	}
	if !strings.Contains(rec.Body.String(), `"children":[]`) {
		t.Errorf("children must serialize as [] not null; body=%s", rec.Body.String())
	}
}

// An unknown lens is 404 (from LensMiddleware, before the handler runs).
func TestLensTopics_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	s := &Server{Manager: m, OntologyRoot: "kb", providers: storeProviders{topicLister: &lensTopicsStub{}}}
	r := s.NewAPIRouter()

	rec := getLensFacts(t, r, "/lenses/missing/topics")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
