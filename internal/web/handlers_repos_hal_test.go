package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// newTestManagerWithRepos builds a repos.Manager and registers empty
// RepoInstance stubs under the given names. Tests use this to verify the
// handler's collection shape without spinning up real stores.
func newTestManagerWithRepos(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	for _, name := range names {
		m.Set(name, &repos.RepoInstance{})
	}
	return m
}

func TestHandleHALRepos_ReturnsCollection(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha", "beta")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Repos []struct {
				Name  string      `json:"name"`
				Links hal.LinkMap `json:"_links"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Count != 2 {
		t.Errorf("count: got %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if len(body.Embedded.Repos) != 2 {
		t.Fatalf("embedded repos: got %d, want 2", len(body.Embedded.Repos))
	}

	// Each embedded item carries ONLY _links.self (hard rule §3 #7).
	for _, repo := range body.Embedded.Repos {
		if len(repo.Links) != 1 {
			t.Errorf("repo %q: got %d links, want 1 (self only)", repo.Name, len(repo.Links))
		}
		if _, ok := repo.Links["self"]; !ok {
			t.Errorf("repo %q: missing self link", repo.Name)
		}
	}
}

func TestHandleHALRepo_ReturnsRepoWithBranchesLink(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	var body struct {
		Name  string      `json:"name"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "alpha" {
		t.Errorf("name: got %q", body.Name)
	}
	for _, rel := range []string{"self", "branches"} {
		if _, ok := body.Links[rel]; !ok {
			t.Errorf("missing link %q", rel)
		}
	}
	if got := body.Links["branches"].Href; got != APIBase+"/repos/alpha/branches" {
		t.Errorf("branches link: %q", got)
	}
}

// TestHandleHALRepo_IncludesDescriptionFromKBMd verifies the single-repo
// response carries the full kb.md content as "description". The default
// kb.md root manifest is "# Knowledge Base\n\nRoot manifest.\n".
func TestHandleHALRepo_IncludesDescriptionFromKBMd(t *testing.T) {
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:         home,
			ClusterCache: config.ClusterCacheConfig{},
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, "work")
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/work", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "work" {
		t.Errorf("name: got %q, want %q", body.Name, "work")
	}
	if !strings.Contains(body.Description, "Root manifest.") {
		t.Errorf("description: got %q, want it to contain the kb.md body", body.Description)
	}
	if !strings.Contains(body.Description, "# Knowledge Base") {
		t.Errorf("description should be the whole kb.md file (incl. heading); got %q", body.Description)
	}
}

// TestHandleHALRepo_OmitsDescriptionWhenNoStore verifies a stub instance with
// no store does not panic and simply omits the description.
func TestHandleHALRepo_OmitsDescriptionWhenNoStore(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"description"`) {
		t.Errorf("description must be omitted when no kb.md is readable; body=%s", rec.Body.String())
	}
}

// TestHandleHALRepo_IncludesShortID verifies the single-repo response carries
// the repo's 12-hex wire id (kb://<id>/… form) as "id", matching ShortID().
func TestHandleHALRepo_IncludesShortID(t *testing.T) {
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:         home,
			ClusterCache: config.ClusterCacheConfig{},
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, "work")
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/work", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.ID) != 12 {
		t.Errorf("id: got %q (len %d), want 12-hex wire form", body.ID, len(body.ID))
	}
	if want := m.Get("work").ShortID(); body.ID != want {
		t.Errorf("id: got %q, want %q (ri.ShortID())", body.ID, want)
	}
}

// TestHandleHALRepos_IncludesShortID verifies each collection item carries the
// repo's 12-hex wire id as "id", matching ShortID().
func TestHandleHALRepos_IncludesShortID(t *testing.T) {
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:         home,
			ClusterCache: config.ClusterCacheConfig{},
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, "work")
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Embedded struct {
			Repos []struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Embedded.Repos) == 0 {
		t.Fatalf("no embedded repos returned")
	}
	for _, repo := range body.Embedded.Repos {
		if len(repo.ID) != 12 {
			t.Errorf("repo %q id: got %q (len %d), want 12-hex wire form", repo.Name, repo.ID, len(repo.ID))
		}
		if want := m.Get(repo.Name).ShortID(); repo.ID != want {
			t.Errorf("repo %q id: got %q, want %q (ri.ShortID())", repo.Name, repo.ID, want)
		}
	}
}

func TestHandleHALRepo_UnknownReturns404Problem(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALRepos_EmptyManagerReturnsEmptyCollection(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Repos []any `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count: got %d, want 0", body.Count)
	}
	if body.Embedded.Repos == nil {
		t.Error("embedded repos should be an empty array, not nil")
	}
}

// initRepoFile creates a new repo .db file under <home>/repos/<name>.db.
// Mirrors the relevant parts of app.InitRepo without importing internal/app
// (which would create a cycle through the MCP/web layer). The default
// ontology is committed so Manager.Add doesn't emit a "not found" warning.
func initRepoFile(t *testing.T, home, name string) {
	t.Helper()
	dbPath := filepath.Join(home, "repos", name+".db")
	svc, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	ontologyYAML, err := fact.DefaultOntology().Serialize()
	if err != nil {
		t.Fatalf("serialize ontology: %v", err)
	}
	if err := svc.InitRepo(map[string]string{
		"domains/ontology.yaml": string(ontologyYAML),
	}, "machine/test"); err != nil {
		t.Fatalf("svc.InitRepo: %v", err)
	}
}

func TestHandleReposRescan_ReturnsAddedAndSkipped(t *testing.T) {
	// Bootstrap a real manager so Rescan has a directory to scan.
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:         home,
			ClusterCache: config.ClusterCacheConfig{},
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	// Start creates nothing — there is no default repo — so make the
	// pre-existing repo the rescan is expected to SKIP explicitly.
	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "existing", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create existing repo: %v", err)
	}

	// Drop a new repo on disk.
	initRepoFile(t, home, "work")

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos:rescan", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Added   []string `json:"added"`
		Skipped []string `json:"skipped"`
		Errors  []struct {
			Repo  string `json:"repo"`
			Error string `json:"error"`
		} `json:"errors"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !slices.Contains(body.Added, "work") {
		t.Errorf("added: %v, want to contain 'work'", body.Added)
	}
	if !slices.Contains(body.Skipped, "existing") {
		t.Errorf("skipped: %v, want to contain %q", body.Skipped, "existing")
	}
	if len(body.Errors) != 0 {
		t.Errorf("errors: %v, want empty", body.Errors)
	}
	if slices.Contains(body.Added, "existing") {
		t.Errorf("an already-open repo must not appear in Added")
	}
	if slices.Contains(body.Skipped, "work") {
		t.Errorf("work must not appear in Skipped (it was newly created)")
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if _, ok := body.Links["repos"]; !ok {
		t.Error("missing repos link")
	}
}

func TestHandleReposRescan_EmptyArraysSerializeAsArray(t *testing.T) {
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg: config.Config{
			Home:         home,
			ClusterCache: config.ClusterCacheConfig{},
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos:rescan", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Verify raw JSON: empty arrays must serialize as [] not null.
	raw := rec.Body.String()
	if !strings.Contains(raw, `"added":[]`) {
		t.Errorf(`expected "added":[], got body=%s`, raw)
	}
	if !strings.Contains(raw, `"errors":[]`) {
		t.Errorf(`expected "errors":[], got body=%s`, raw)
	}
}

func TestHandleHALRepos_IncludesRescanLink(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	var body struct {
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	rescan, ok := body.Links["rescan"]
	if !ok {
		t.Fatalf("missing rescan link; got links=%v", body.Links)
	}
	if rescan.Href != "/api/v1/repos:rescan" {
		t.Errorf("rescan href: got %q, want /api/v1/repos:rescan", rescan.Href)
	}
}

// newRepoPatchServer boots a real manager with one on-disk repo ("work") whose
// kb.md holds the default root manifest, and returns its API router.
func newRepoPatchServer(t *testing.T) http.Handler {
	t.Helper()
	r, _ := newRepoPatchServerWithManager(t)
	return r
}

// newRepoPatchServerWithManager is newRepoPatchServer for tests that must also
// reach past HTTP — asserting on the git ref itself, which no read endpoint
// exposes unfiltered (the commits list is scoped to the ontology root, so a
// root-level kb.md commit never appears there).
func newRepoPatchServerWithManager(t *testing.T) (http.Handler, *repos.Manager) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home, ClusterCache: config.ClusterCacheConfig{}},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	initRepoFile(t, home, "work")
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	s := &Server{Manager: m, AgentBranch: "machine/test"}
	return s.NewAPIRouter(), m
}

func patchRepo(t *testing.T, r http.Handler, repo, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/"+repo, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func repoDescription(t *testing.T, r http.Handler, repo string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/"+repo, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body.Description
}

// A PATCHed description is committed to kb.md and is visible to the very next
// GET — the write and the read must agree on file AND branch, or the edit
// silently disappears.
func TestHandleHALRepoPatch_WritesKBMdAndRoundTrips(t *testing.T) {
	r := newRepoPatchServer(t)

	const md = "# Work\n\nA **markdown** manifest.\n\n- one\n- two\n"
	rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"description": md}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The PATCH response is the re-read view, not an echo of the request.
	var patched struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if patched.Name != "work" {
		t.Errorf("name: got %q", patched.Name)
	}
	if patched.Description != md {
		t.Errorf("PATCH description: got %q, want %q", patched.Description, md)
	}
	if got := repoDescription(t, r, "work"); got != md {
		t.Errorf("GET after PATCH: got %q, want %q", got, md)
	}
}

// Markdown is stored verbatim — no normalization, no trailing-newline fixups.
func TestHandleHALRepoPatch_StoresMarkdownVerbatim(t *testing.T) {
	r := newRepoPatchServer(t)
	const md = "|a|b|\n|---|---|\n|1|2|"
	if rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"description": md})); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != md {
		t.Errorf("got %q, want %q (byte-identical)", got, md)
	}
}

// An omitted description is a no-op, not a clear — PATCH is a merge.
func TestHandleHALRepoPatch_OmittedDescriptionKeepsCurrent(t *testing.T) {
	r := newRepoPatchServer(t)
	before := repoDescription(t, r, "work")
	if before == "" {
		t.Fatal("precondition: seeded repo should have a kb.md description")
	}
	if rec := patchRepo(t, r, "work", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != before {
		t.Errorf("omitted description changed the manifest: got %q, want %q", got, before)
	}
}

// An explicit empty string clears the manifest — the description then drops out
// of the GET body entirely (readKBManifest treats "" as absent).
func TestHandleHALRepoPatch_EmptyDescriptionClears(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"description":""}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != "" {
		t.Errorf("description should be cleared; got %q", got)
	}
}

// Over-cap descriptions are refused with 422, and the stored manifest is
// untouched — a rejected write must not partially land.
func TestHandleHALRepoPatch_RejectsOversizeDescription(t *testing.T) {
	r := newRepoPatchServer(t)
	before := repoDescription(t, r, "work")

	body := mustJSON(t, map[string]any{"description": strings.Repeat("x", repos.MaxRepoDescriptionBytes+1)})
	rec := patchRepo(t, r, "work", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != before {
		t.Errorf("rejected write must not change the manifest; got %q", got)
	}

	// Exactly at the cap is accepted.
	atCap := mustJSON(t, map[string]any{"description": strings.Repeat("x", repos.MaxRepoDescriptionBytes)})
	if rec := patchRepo(t, r, "work", atCap); rec.Code != http.StatusOK {
		t.Fatalf("at-cap status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// A body far larger than any legitimate manifest is refused by the reader
// before it is buffered, as a 413 rather than a decode error.
func TestHandleHALRepoPatch_RejectsOversizeBody(t *testing.T) {
	r := newRepoPatchServer(t)
	body := mustJSON(t, map[string]any{"description": strings.Repeat("x", maxRepoPatchBodyBytes+1)})
	if rec := patchRepo(t, r, "work", body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

// Re-saving byte-identical content must not append an empty commit: the store's
// write path always builds a fresh commit object, so an unchanged Save would
// otherwise grow the agent branch (and push) for nothing.
func TestHandleHALRepoPatch_UnchangedDescriptionMakesNoCommit(t *testing.T) {
	r, m := newRepoPatchServerWithManager(t)

	const md = "# Work\n\nManifest.\n"
	if rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"description": md})); rec.Code != http.StatusOK {
		t.Fatalf("seed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	before := repoHeadCommit(t, m, "work")

	// Same bytes again — accepted, but the branch tip must not move.
	if rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"description": md})); rec.Code != http.StatusOK {
		t.Fatalf("no-op PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoHeadCommit(t, m, "work"); got != before {
		t.Errorf("unchanged description committed: HEAD moved %s → %s", before, got)
	}
	if got := repoDescription(t, r, "work"); got != md {
		t.Errorf("description after no-op PATCH: got %q, want %q", got, md)
	}

	// A real change still commits.
	if rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"description": md + "More.\n"})); rec.Code != http.StatusOK {
		t.Fatalf("changed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoHeadCommit(t, m, "work"); got == before {
		t.Error("a changed description must produce a commit; HEAD did not move")
	}
}

// repoHeadCommit returns the agent branch's tip commit hash straight from the
// store, so a test can assert whether a request produced a commit.
func repoHeadCommit(t *testing.T, m *repos.Manager, repo string) string {
	t.Helper()
	ri := m.Get(repo)
	if ri == nil {
		t.Fatalf("repo %q not registered", repo)
	}
	var head string
	if err := ri.WithRead(func(svc *store.Service) {
		h, herr := svc.Branches().HeadCommit(context.Background(), "machine/test")
		if herr != nil {
			t.Errorf("head commit: %v", herr)
			return
		}
		head = h
	}); err != nil {
		t.Fatalf("with read: %v", err)
	}
	return head
}

// Malformed JSON is a 400, not a 500.
func TestHandleHALRepoPatch_RejectsMalformedBody(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"description":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// An unknown repo 404s via the repo middleware, before any write is attempted.
func TestHandleHALRepoPatch_UnknownRepo404s(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "nope", `{"description":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// newHALTestEnvWithCredential builds a real, started repos.Manager whose
// registry has a working Crypt — SetOriginCredential refuses to store a token
// without one, so a stub manager (newTestManagerWithRepos) can't exercise this
// path. It registers one active repo named `name` with a token origin, plus a
// second, unrelated repo that is immediately archived, so both /repos and
// /archived are non-empty for TestNoCredentialEverReachesTheAPI to inspect.
func newHALTestEnvWithCredential(t *testing.T, name, secret string) *Server {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	keyPath := filepath.Join(home, "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))

	m := repos.New(ctx, repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.Create(ctx, repos.CreateSpec{Name: name, Mode: "preset"}, nil)
	require.NoError(t, err)
	require.NoError(t, m.SetOrigin(ctx, name,
		repos.OriginSpec{URL: "https://example.invalid/repo.git", Branch: "main", AuthMethod: "token", AuthToken: secret},
		300, 300))

	_, err = m.Create(ctx, repos.CreateSpec{Name: "sidecar", Mode: "preset"}, nil)
	require.NoError(t, err)
	_, err = m.Archive("sidecar")
	require.NoError(t, err)

	return &Server{Manager: m, AgentBranch: "machine/test"}
}

// TestNoCredentialEverReachesTheAPI asserts at the byte level, so it catches a
// future field copy regardless of which struct grows the field.
func TestNoCredentialEverReachesTheAPI(t *testing.T) {
	const secret = "sup3r-s3cret-token"
	s := newHALTestEnvWithCredential(t, "work", secret)
	r := s.NewAPIRouter()

	for _, path := range []string{"/repos", "/archived"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.NotContains(t, rec.Body.String(), secret,
			"%s must never carry a credential", path)
	}
}
