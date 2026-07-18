package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if !slices.Contains(body.Skipped, config.DefaultRepoName) {
		t.Errorf("skipped: %v, want to contain %q", body.Skipped, config.DefaultRepoName)
	}
	if len(body.Errors) != 0 {
		t.Errorf("errors: %v, want empty", body.Errors)
	}
	if slices.Contains(body.Added, config.DefaultRepoName) {
		t.Errorf("default repo must not appear in Added (it was pre-existing)")
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
