package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
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

// The repo collection carries each repo's registry uid. This is the only place a
// client can learn it, and the lens API accepts nothing else for a member — the
// 400 for an unknown member sends callers here by name.
func TestHandleHALRepos_CarryRegistryUID(t *testing.T) {
	// A really-provisioned repo, not the bare RepoInstance the other tests in
	// this file use: the point is the uid the registry assigned it.
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	var body struct {
		Embedded struct {
			Repos []struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
				ID   string `json:"id"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(body.Embedded.Repos) != 1 {
		t.Fatalf("repos: got %d, want 1; body=%s", len(body.Embedded.Repos), rec.Body.String())
	}
	got := body.Embedded.Repos[0]
	want := m.Get("alpha").UID()
	if got.UID != want {
		t.Errorf("uid: got %q, want %q", got.UID, want)
	}
	// uid is not the root-commit id: they answer different questions and a
	// client that confused them would address the wrong repo.
	if got.UID == got.ID {
		t.Errorf("uid must be distinct from the root-commit id (both %q)", got.UID)
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

// TestHandleHALRepo_IncludesDescriptionFromReadme verifies the single-repo
// response carries the full README.md content as "description". InitRepo
// seeds README.md, so a freshly created repo already has one — no PATCH
// needed to seed it.
func TestHandleHALRepo_IncludesDescriptionFromReadme(t *testing.T) {
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
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
		t.Errorf("description: got %q, want it to contain the README.md body", body.Description)
	}
	if !strings.Contains(body.Description, "# Knowledge Base") {
		t.Errorf("description should be the whole README.md file (incl. heading); got %q", body.Description)
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
		t.Errorf("description must be omitted when no README.md is readable; body=%s", rec.Body.String())
	}
}

// TestHandleHALRepo_IncludesLicenseWhenPresent verifies the single-repo
// response carries a root LICENSE as "license", verbatim. InitRepo does not
// seed one, so this writes it directly through the fact store the same way
// the manifest package's own tests do.
func TestHandleHALRepo_IncludesLicenseWhenPresent(t *testing.T) {
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
	}

	ri := m.Get("work")
	require.NotNil(t, ri)
	const mit = "MIT License\n\nPermission is hereby granted, free of charge...\n"
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteRootFile(context.Background(), ri.AgentBranch(),
			"LICENSE", mit, "docs: add LICENSE", "update")
		require.NoError(t, werr)
	}))

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/work", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		License string `json:"license"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.License != mit {
		t.Errorf("license: got %q, want %q", body.License, mit)
	}
}

// TestHandleHALRepo_ReportsLicenseOversize verifies that a LICENSE exceeding
// repos.MaxRepoDescriptionBytes is reported as "license_oversize": true with
// no "license" field, NOT as an absent licence. ReadFact enforces no size cap
// of its own, so it is possible for a >64KiB LICENSE to land on the branch
// (a clone, or a hand-edited working tree — WriteLicense itself rejects
// oversized input at the door). Before this flag existed, repoView had no way
// to say anything but "absent" for that file, and the UI offered "Add
// license" over content it could not actually read.
func TestHandleHALRepo_ReportsLicenseOversize(t *testing.T) {
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
	}

	ri := m.Get("work")
	require.NotNil(t, ri)
	oversized := strings.Repeat("x", repos.MaxRepoDescriptionBytes+1)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteRootFile(context.Background(), ri.AgentBranch(),
			"LICENSE", oversized, "docs: add oversize LICENSE", "update")
		require.NoError(t, werr)
	}))

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/work", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		License         string `json:"license"`
		LicenseOversize bool   `json:"license_oversize"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.LicenseOversize {
		t.Errorf("license_oversize: got false, want true; body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"license"`) {
		t.Errorf("an oversize licence must not also appear under \"license\"; body=%s", rec.Body.String())
	}
}

// TestHandleHALRepo_OmitsLicenseWhenNoStore verifies a stub instance with no
// store does not panic and simply omits the license, exactly like
// TestHandleHALRepo_OmitsDescriptionWhenNoStore does for description.
func TestHandleHALRepo_OmitsLicenseWhenNoStore(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"license"`) {
		t.Errorf("license must be omitted when no LICENSE is readable; body=%s", rec.Body.String())
	}
}

// TestHandleHALRepos_OmitsLicenseFromList verifies the repo LIST never carries
// a "license" field, even when the repo has one — the collection must stay a
// cheap index and not grow a second per-repo git read.
func TestHandleHALRepos_OmitsLicenseFromList(t *testing.T) {
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
	}

	ri := m.Get("work")
	require.NotNil(t, ri)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteRootFile(context.Background(), ri.AgentBranch(),
			"LICENSE", "MIT License\n", "docs: add LICENSE", "update")
		require.NoError(t, werr)
	}))

	s := &Server{Manager: m, AgentBranch: "machine/test"}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"license"`) {
		t.Errorf("license must never appear in the repo LIST; body=%s", rec.Body.String())
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
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

	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
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

// newRepoPatchServer boots a real manager with one on-disk repo ("work") whose
// README.md holds the default root manifest, and returns its API router.
func newRepoPatchServer(t *testing.T) http.Handler {
	t.Helper()
	r, _ := newRepoPatchServerWithManager(t)
	return r
}

// newRepoPatchServerWithManager is newRepoPatchServer for tests that must also
// reach past HTTP — asserting on the git ref itself, which no read endpoint
// exposes unfiltered (the commits list is scoped to the ontology root, so a
// root-level README.md commit never appears there).
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
	if _, err := m.Create(context.Background(), repos.CreateSpec{Name: "work", Mode: "preset"}, nil); err != nil {
		t.Fatalf("create work: %v", err)
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

// repoLicense is repoDescription's sibling: the licence as the GET reports it,
// "" when the field is absent.
func repoLicense(t *testing.T, r http.Handler, repo string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/"+repo, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		License string `json:"license"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body.License
}

// A PATCHed description is committed to README.md and is visible to the very next
// GET — the write and the read must agree on file AND branch, or the edit
// silently disappears.
func TestHandleHALRepoPatch_WritesReadmeAndRoundTrips(t *testing.T) {
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
		t.Fatal("precondition: seeded repo should have a README.md description")
	}
	if rec := patchRepo(t, r, "work", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != before {
		t.Errorf("omitted description changed the manifest: got %q, want %q", got, before)
	}
}

// An explicit empty string clears the manifest — the description then drops out
// of the GET body entirely (readReadme treats "" as absent).
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

// A PATCHed licence is committed to LICENSE and visible to the very next GET.
// Newlines survive verbatim — the whole point of the file.
func TestHandleHALRepoPatch_WritesLicenseAndRoundTrips(t *testing.T) {
	r := newRepoPatchServer(t)
	const mit = "MIT License\n\nCopyright (c) 2026\n\n* not a bullet\n"

	if rec := patchRepo(t, r, "work", mustJSON(t, map[string]any{"license": mit})); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoLicense(t, r, "work"); got != mit {
		t.Errorf("licence round-trip: got %q, want %q", got, mit)
	}
}

// The two fields are independent: patching one must not disturb the other.
func TestHandleHALRepoPatch_OmittedLicenseKeepsCurrent(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"license":"terms\n"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	beforeDesc := repoDescription(t, r, "work")

	if rec := patchRepo(t, r, "work", `{"description":"new text\n"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoLicense(t, r, "work"); got != "terms\n" {
		t.Errorf("a description-only patch changed the licence: got %q", got)
	}
	if beforeDesc == "" {
		t.Fatal("precondition: seeded repo should have a README.md description")
	}
}

// An explicit empty string clears the file — it then drops out of the GET body
// entirely, exactly as an emptied description does.
func TestHandleHALRepoPatch_EmptyLicenseClears(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"license":"terms\n"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := patchRepo(t, r, "work", `{"license":""}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoLicense(t, r, "work"); got != "" {
		t.Errorf("licence should be cleared; got %q", got)
	}
}

// Over-cap licences are refused with 422 and the stored file is untouched — a
// rejected write must not partially land. At the cap exactly is accepted.
func TestHandleHALRepoPatch_RejectsOversizeLicense(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"license":"terms\n"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}

	body := mustJSON(t, map[string]any{"license": strings.Repeat("x", repos.MaxRepoDescriptionBytes+1)})
	if rec := patchRepo(t, r, "work", body); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoLicense(t, r, "work"); got != "terms\n" {
		t.Errorf("rejected write must not change the licence; got %q", got)
	}

	atCap := mustJSON(t, map[string]any{"license": strings.Repeat("x", repos.MaxRepoDescriptionBytes)})
	if rec := patchRepo(t, r, "work", atCap); rec.Code != http.StatusOK {
		t.Fatalf("at-cap status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleHALRepoPatch_RefusesToReplaceOversizeLicense is the write-side
// half of TestHandleHALRepo_ReportsLicenseOversize: an existing LICENSE over
// the cap must be refused with 409 (the resource is in a state this request
// cannot safely act on), both for a blank "clear" draft and for genuine new
// terms — and the original bytes on the branch must survive untouched
// either way. This is the exact trap the oversize flag exists to close: a UI
// that could not display the file must not be able to erase it either.
func TestHandleHALRepoPatch_RefusesToReplaceOversizeLicense(t *testing.T) {
	r, m := newRepoPatchServerWithManager(t)
	ri := m.Get("work")
	require.NotNil(t, ri)
	oversized := strings.Repeat("x", repos.MaxRepoDescriptionBytes+1)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteRootFile(context.Background(), ri.AgentBranch(),
			"LICENSE", oversized, "docs: add oversize LICENSE", "update")
		require.NoError(t, werr)
	}))

	for _, attempt := range []string{"", "new terms\n"} {
		body := mustJSON(t, map[string]any{"license": attempt})
		rec := patchRepo(t, r, "work", body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("license=%q: status got %d, want 409; body=%s", attempt, rec.Code, rec.Body.String())
		}
		require.NoError(t, ri.WithRead(func(svc *store.Service) {
			res, rerr := svc.Facts().ReadFact(context.Background(), ri.AgentBranch(), repos.LicensePath, nil)
			require.NoError(t, rerr)
			if res.Content != oversized {
				t.Errorf("license=%q: the original LICENSE must survive untouched", attempt)
			}
		}))
	}
}

// A two-field PATCH is validated BEFORE either write. The handler commits
// README.md and LICENSE as two separate git commits with no transaction across
// them, so a body with a fine description and an over-cap licence used to
// commit the description and only then answer 422 — the client sees an error,
// reasonably concludes nothing happened, and is wrong.
//
// Both caps are knowable up front (they are the same cap), so the fix is to
// check both lengths before touching git. The assertion that matters is not the
// status code — that was always 422 — but that the DESCRIPTION IS UNCHANGED
// afterwards.
func TestHandleHALRepoPatch_OversizeLicenseDoesNotLandTheDescription(t *testing.T) {
	r := newRepoPatchServer(t)
	before := repoDescription(t, r, "work")
	if before == "" {
		t.Fatal("precondition: seeded repo should have a README.md description")
	}

	body := mustJSON(t, map[string]any{
		"description": "this description is perfectly fine\n",
		"license":     strings.Repeat("x", repos.MaxRepoDescriptionBytes+1),
	})
	rec := patchRepo(t, r, "work", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != before {
		t.Errorf("the rejected patch half-landed: description is now %q, want %q (unchanged)", got, before)
	}
	if got := repoLicense(t, r, "work"); got != "" {
		t.Errorf("the rejected licence must not have landed either; got %q", got)
	}
}

// The mirror: an over-cap DESCRIPTION alongside a fine licence must leave the
// licence alone too. That direction already held (the description is written
// first, so its rejection precedes the licence write), and this pins it so a
// future reordering of the two writes cannot quietly break it.
func TestHandleHALRepoPatch_OversizeDescriptionDoesNotLandTheLicense(t *testing.T) {
	r := newRepoPatchServer(t)
	if rec := patchRepo(t, r, "work", `{"license":"terms\n"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed PATCH status: %d; body=%s", rec.Code, rec.Body.String())
	}

	body := mustJSON(t, map[string]any{
		"description": strings.Repeat("x", repos.MaxRepoDescriptionBytes+1),
		"license":     "brand new terms\n",
	})
	rec := patchRepo(t, r, "work", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoLicense(t, r, "work"); got != "terms\n" {
		t.Errorf("the rejected patch half-landed: licence is now %q, want %q (unchanged)", got, "terms\n")
	}
}

// Both fields within the cap still apply BOTH — the pre-check must not have
// become a gate that quietly drops one of them.
func TestHandleHALRepoPatch_BothFieldsApplyTogether(t *testing.T) {
	r := newRepoPatchServer(t)
	body := mustJSON(t, map[string]any{
		"description": "# Work\n\nmanifest\n",
		"license":     "MIT License\n",
	})
	if rec := patchRepo(t, r, "work", body); rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := repoDescription(t, r, "work"); got != "# Work\n\nmanifest\n" {
		t.Errorf("description: got %q", got)
	}
	if got := repoLicense(t, r, "work"); got != "MIT License\n" {
		t.Errorf("licence: got %q", got)
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

// newManagerWithMissingRepoFile boots a manager, creates the named repos, then
// reboots a SECOND manager over the same home with the FIRST repo's database
// deleted. The reboot is what classifies it: openRegistered stats the file,
// finds nothing, and records it as unavailable/"missing" instead of dropping it.
//
// Returns the rebooted manager and the missing repo's uid.
func newManagerWithMissingRepoFile(t *testing.T, missing string, alsoCreate ...string) (*repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	newMgr := func() *repos.Manager {
		return repos.New(context.Background(), repos.Deps{
			Cfg:                   config.Config{Home: home},
			AgentBranch:           "machine/test",
			DisableBackgroundSync: true,
		})
	}

	m := newMgr()
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: missing, Mode: "preset"}, nil)
	if err != nil {
		t.Fatalf("create %q: %v", missing, err)
	}
	uid := ri.UID()
	for _, name := range alsoCreate {
		if _, cerr := m.Create(context.Background(), repos.CreateSpec{Name: name, Mode: "preset"}, nil); cerr != nil {
			t.Fatalf("create %q: %v", name, cerr)
		}
	}
	if cerr := m.Close(); cerr != nil {
		t.Fatalf("manager close: %v", cerr)
	}
	if rerr := os.Remove(m.RepoPath(uid)); rerr != nil {
		t.Fatalf("remove %q database: %v", missing, rerr)
	}

	m2 := newMgr()
	if err := m2.Start(); err != nil {
		t.Fatalf("manager restart: %v", err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	if m2.Get(missing) != nil {
		t.Fatalf("sanity: %q must have no live store after its database was deleted", missing)
	}
	return m2, uid
}

// A registered repo whose database is gone stays LISTED, carrying the reason it
// has no store, and its endpoints answer 409 rather than 404: it IS registered,
// so "no such repo" would be a lie. Before control.db owned the repo list such a
// repo vanished from the API entirely, with one ERROR log line as its trace.
func TestListRepos_IncludesUnavailable(t *testing.T) {
	m, uid := newManagerWithMissingRepoFile(t, "core", "alive")
	r := (&Server{Manager: m, AgentBranch: "machine/test"}).NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Repos []struct {
				Name   string      `json:"name"`
				UID    string      `json:"uid"`
				ID     string      `json:"id"`
				State  string      `json:"state"`
				Detail string      `json:"detail"`
				Links  hal.LinkMap `json:"_links"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if body.Count != 2 || len(body.Embedded.Repos) != 2 {
		t.Fatalf("repos: count=%d items=%d, want 2 (the live one AND the unavailable one); body=%s",
			body.Count, len(body.Embedded.Repos), rec.Body.String())
	}
	// One sorted list, not "live repos then the broken ones tacked on".
	if got := []string{body.Embedded.Repos[0].Name, body.Embedded.Repos[1].Name}; got[0] != "alive" || got[1] != "core" {
		t.Fatalf("order: got %v, want [alive core] (one list, sorted by name)", got)
	}

	alive, broken := body.Embedded.Repos[0], body.Embedded.Repos[1]
	if alive.State != "active" {
		t.Errorf("live repo state: got %q, want %q", alive.State, "active")
	}
	if alive.Detail != "" {
		t.Errorf("live repo detail: got %q, want it omitted", alive.Detail)
	}
	if broken.State != "missing" {
		t.Errorf("unavailable repo state: got %q, want %q", broken.State, "missing")
	}
	if broken.UID != uid {
		t.Errorf("unavailable repo uid: got %q, want %q", broken.UID, uid)
	}
	// The root-commit id is a property of a store this repo does not have, so it
	// must be empty rather than a plausible-looking lie.
	if broken.ID != "" {
		t.Errorf("unavailable repo id: got %q, want empty (it has never been opened)", broken.ID)
	}
	if broken.Detail == "" {
		t.Error("unavailable repo must carry a human-readable detail")
	}
	if _, ok := broken.Links["self"]; !ok {
		t.Error("unavailable repo must still carry a self link — following it is how you learn why")
	}

	// Every route behind the repo middleware answers 409, and the body names the
	// reason: "the file is gone" and "another repo holds this knowledge base"
	// call for different fixes, so a bare 409 would not be enough.
	for _, path := range []string{
		"/repos/core",
		"/repos/core/branches/machine:test/facts",
	} {
		prec := httptest.NewRecorder()
		r.ServeHTTP(prec, httptest.NewRequest(http.MethodGet, path, nil))
		if prec.Code != http.StatusConflict {
			t.Fatalf("%s: got %d, want 409 (it is registered, so 404 would be wrong); body=%s",
				path, prec.Code, prec.Body.String())
		}
		if got := prec.Header().Get("Content-Type"); got != "application/problem+json" {
			t.Errorf("%s content-type: got %q", path, got)
		}
		var p map[string]any
		if err := json.Unmarshal(prec.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s: unmarshal problem: %v; body=%s", path, err, prec.Body.String())
		}
		detail, _ := p["detail"].(string)
		if !strings.Contains(detail, "missing") {
			t.Errorf("%s detail: got %q, want it to name the reason (%q)", path, detail, "missing")
		}
	}

	// A genuinely unregistered name is still a 404 — the 409 must not swallow it.
	nrec := httptest.NewRecorder()
	r.ServeHTTP(nrec, httptest.NewRequest(http.MethodGet, "/repos/nosuchrepo", nil))
	if nrec.Code != http.StatusNotFound {
		t.Errorf("unregistered name: got %d, want 404", nrec.Code)
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
