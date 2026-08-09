package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// newTestLensManager builds a started repos.Manager (so Registry() is live)
// and provisions the named preset repos. It returns the manager and its Home
// dir so replica tests can copy a repo's .db to force an ID collision.
func newTestLensManager(t *testing.T, names ...string) (*repos.Manager, string) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, name := range names {
		if _, err := m.Create(context.Background(), repos.CreateSpec{
			Name: name, Mode: "preset", OntologyPreset: "default",
		}, nil); err != nil {
			t.Fatalf("create repo %q: %v", name, err)
		}
	}
	return m, home
}

// cloneRepo provisions a replica of src under dst by copying its .db, so both
// resolve to the same root-commit ID — the shape ValidateLens must reject.
func cloneRepo(t *testing.T, m *repos.Manager, home, src, dst string) {
	t.Helper()
	srcRI := m.Get(src)
	if srcRI == nil {
		t.Fatalf("source repo %q not found", src)
	}
	srcRI.WithRead(func(svc *store.Service) {
		if svc == nil {
			t.Fatalf("source repo %q has no store", src)
		}
		if err := svc.Checkpoint(); err != nil {
			t.Fatalf("checkpoint %q: %v", src, err)
		}
	})
	reposDir := filepath.Join(home, "repos")
	data, err := os.ReadFile(m.RepoPath(srcRI.UID()))
	if err != nil {
		t.Fatalf("read src db: %v", err)
	}
	dstPath := filepath.Join(reposDir, dst+".db")
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		t.Fatalf("write dst db: %v", err)
	}
	if err := m.Add(dst, "", dstPath, nil); err != nil {
		t.Fatalf("add clone %q: %v", dst, err)
	}
}

// lensViewBody mirrors the wire shape of a single lens for decoding in tests.
type lensViewBody struct {
	Name  string `json:"name"`
	Write string `json:"write"`
	Reads []struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Source string `json:"source"`
	} `json:"reads"`
	Description string      `json:"description"`
	CreatedAt   int64       `json:"created_at"`
	UpdatedAt   int64       `json:"updated_at"`
	Links       hal.LinkMap `json:"_links"`
}

func postLens(t *testing.T, r http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/lenses", bytes.NewBufferString(body))
	r.ServeHTTP(rec, req)
	return rec
}

func TestHandleHALLensesCreate_Created(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "eng" || body.Write != "alpha" {
		t.Errorf("name/write: got %q/%q", body.Name, body.Write)
	}
	// normalize() folds the write repo into the reads, so both members appear.
	if len(body.Reads) != 2 {
		t.Fatalf("reads: got %d, want 2 (write repo folded in); body=%s", len(body.Reads), rec.Body.String())
	}
	if body.Links["self"].Href != APIBase+"/lenses/eng" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}

	// Proof CreateLens persisted it (so validation actually ran).
	got, ok, err := m.Registry().Get("eng")
	if err != nil || !ok {
		t.Fatalf("registry Get: ok=%v err=%v", ok, err)
	}
	if got.Write != "alpha" {
		t.Errorf("persisted write: got %q", got.Write)
	}
}

// A description POSTed on create is persisted and returned by both the single
// GET and the list GET.
func TestHandleHALLensesCreate_DescriptionRoundTrips(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}],"description":"team knowledge base"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.Description != "team knowledge base" {
		t.Errorf("create description: got %q", created.Description)
	}

	// Single GET.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
	var single lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("unmarshal single: %v", err)
	}
	if single.Description != "team knowledge base" {
		t.Errorf("single-GET description: got %q", single.Description)
	}

	// List GET.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
	var list struct {
		Embedded struct {
			Lenses []lensViewBody `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Embedded.Lenses) != 1 || list.Embedded.Lenses[0].Description != "team knowledge base" {
		t.Errorf("list description: got %+v", list.Embedded.Lenses)
	}
}

// A description over the 4 KiB cap is a well-formed request the server refuses
// → 422 Unprocessable Entity.
func TestHandleHALLensesCreate_DescriptionTooLong(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	desc := strings.Repeat("x", 4097)
	body, err := json.Marshal(map[string]any{
		"name": "eng", "write": "alpha",
		"reads":       []map[string]string{{"repo": "beta"}},
		"description": desc,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postLens(t, r, string(body))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}

	// A description exactly at the cap is accepted.
	body, _ = json.Marshal(map[string]any{
		"name": "eng2", "write": "alpha",
		"reads":       []map[string]string{{"repo": "beta"}},
		"description": strings.Repeat("x", 4096),
	})
	if rec := postLens(t, r, string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("at-cap status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_RepoNameCollision(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"beta","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensesCreate_UnknownReadRepo(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"ghost"}]}`)
	// Unknown member repo is a well-formed request naming a nonexistent
	// resource → 422 Unprocessable Entity (pinned; see report).
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_Replica(t *testing.T) {
	m, home := newTestLensManager(t, "alpha")
	cloneRepo(t, m, home, "alpha", "alpha_clone")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"alpha_clone"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_InvalidName(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{"name":"Bad Name","write":"alpha"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_EmptyWrite(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	// Empty write repo → ErrLensWriteEmpty → 400 (A1). Previously this leaked
	// through member resolution as ErrRepoNotFound → 422.
	rec := postLens(t, r, `{"name":"eng","write":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensesCreate_DuplicateName(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}
	// Re-creating the same lens name → ErrLensExists → 409 (backlog C.11).
	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_BadBranchPin(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	// A branch pin the member repo does not have → ErrLensBranchUnknown → 422
	// (backlog C.11).
	rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta","branch":"nope"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensesCreate_BadJSON(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLens(t, r, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLenses_ListShowsCreated(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q", got)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Lenses []lensViewBody `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 || len(body.Embedded.Lenses) != 1 {
		t.Fatalf("count/embedded: got %d/%d, want 1/1; body=%s", body.Count, len(body.Embedded.Lenses), rec.Body.String())
	}
	if body.Embedded.Lenses[0].Name != "eng" {
		t.Errorf("lens name: got %q", body.Embedded.Lenses[0].Name)
	}
	if body.Links["self"].Href != APIBase+"/lenses" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}
}

func TestHandleHALLenses_EmptyCollection(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Lenses []any `json:"lenses"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count: got %d, want 0", body.Count)
	}
	if body.Embedded.Lenses == nil {
		t.Error("embedded lenses should be [] not null")
	}
}

func TestHandleHALLens_GetAndNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/lenses/eng", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "eng" {
		t.Errorf("name: got %q", body.Name)
	}
	if body.Links["self"].Href != APIBase+"/lenses/eng" {
		t.Errorf("self link: got %q", body.Links["self"].Href)
	}

	// Unknown → 404 problem.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/lenses/missing", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensDelete_DeleteAndNotFound(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()

	if rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/lenses/eng", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := m.Registry().Get("eng"); ok {
		t.Error("lens should be gone after delete")
	}

	// Deleting an unknown lens → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/lenses/missing", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: got %d, want 404", rec.Code)
	}
}

// problemDetail decodes the "detail" field of a problem+json body.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal problem: %v; body=%s", err, rec.Body.String())
	}
	return p.Detail
}

// TestLensCreateErrStatus pins the sentinel→(status,title) mapping. The
// ErrCreateInFlight arm is the FIX-1 case: a concurrent same-name create (or a
// lens create racing a repo create for the same name) must map to 409, not the
// 500 default. This mapper is the only deterministic way to exercise that arm —
// the in-flight reservation seam (reserveNameAndOrigin) is unexported.
func TestLensCreateErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", repos.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write empty", repos.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", repos.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", repos.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Create lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensCreateErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}

// TestLensPatchErrStatus pins the PATCH mapping (m13): the validation arms are
// byte-identical to lensCreateErrStatus, but the scrubbed-500 default arm carries
// an operation-appropriate title so a PATCH failure never reads "Create lens
// failed".
func TestLensPatchErrStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantTitle  string
	}{
		// 4xx/422 arms mirror create exactly (byte-identity is a hard constraint).
		{"invalid name", repos.ErrInvalidLensName, http.StatusBadRequest, "Invalid lens name"},
		{"name conflicts repo", repos.ErrLensNameConflictsRepo, http.StatusConflict, "Lens name conflicts with a repo"},
		{"lens exists", repos.ErrLensExists, http.StatusConflict, "Lens already exists"},
		{"create in flight", repos.ErrCreateInFlight, http.StatusConflict, "Create in flight"},
		{"replica in lens", repos.ErrReplicaInLens, http.StatusConflict, "Replica mounts not allowed"},
		{"repo not found", repos.ErrRepoNotFound, http.StatusUnprocessableEntity, "Lens references an unknown repo"},
		{"branch unknown", repos.ErrLensBranchUnknown, http.StatusUnprocessableEntity, "Lens pins an unknown branch"},
		{"write empty", repos.ErrLensWriteEmpty, http.StatusBadRequest, "Lens write repo required"},
		{"description too long", repos.ErrLensDescriptionTooLong, http.StatusUnprocessableEntity, "Lens description too long"},
		{"lens not found", repos.ErrLensNotFound, http.StatusNotFound, "Lens not found"},
		// The only divergence: the unmapped 500 default arm names the PATCH op.
		{"unmapped", errors.New("boom"), http.StatusInternalServerError, "Update lens failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, title := lensPatchErrStatus(tc.err)
			if status != tc.wantStatus || title != tc.wantTitle {
				t.Errorf("got (%d, %q), want (%d, %q)", status, title, tc.wantStatus, tc.wantTitle)
			}
		})
	}
}

// TestHandleHALLenses_500DoesNotLeakError forces a real registry-layer failure
// (closing the control-plane DB while Registry() stays non-nil) and asserts the
// 500 problem detail is a generic string, never the wrapped SQL/driver error
// (FIX 2). Each lens handler's 500 fall-through is covered.
func TestHandleHALLenses_500DoesNotLeakError(t *testing.T) {
	leak := "database is closed" // substring of the raw sql driver error

	t.Run("list", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		if err := m.Registry().Close(); err != nil {
			t.Fatalf("close registry: %v", err)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "list lenses failed" || strings.Contains(d, leak) {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})

	t.Run("get", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		if err := m.Registry().Close(); err != nil {
			t.Fatalf("close registry: %v", err)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "get lens failed" || strings.Contains(d, leak) {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})

	t.Run("delete", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha")
		r := (&Server{Manager: m}).NewAPIRouter()
		if err := m.Registry().Close(); err != nil {
			t.Fatalf("close registry: %v", err)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/lenses/eng", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		// The delete handler's Get-check fails first, so the detail is the
		// get-generic; either way it must not leak the driver error.
		if d := problemDetail(t, rec); strings.Contains(d, leak) {
			t.Errorf("detail leaked: %q", d)
		}
	})

	t.Run("create", func(t *testing.T) {
		m, _ := newTestLensManager(t, "alpha", "beta")
		r := (&Server{Manager: m}).NewAPIRouter()
		if err := m.Registry().Close(); err != nil {
			t.Fatalf("close registry: %v", err)
		}
		rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if d := problemDetail(t, rec); d != "create lens failed" || strings.Contains(d, leak) {
			t.Errorf("detail leaked or unexpected: %q", d)
		}
	})
}

func TestHandleHALLenses_RegistryNil503(t *testing.T) {
	// A manager that was never Started has a nil registry.
	m := repos.New(context.Background(), repos.Deps{})
	r := (&Server{Manager: m}).NewAPIRouter()

	// list
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list: got %d, want 503", rec.Code)
	}
	// get
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("get: got %d, want 503", rec.Code)
	}
	// create
	rec = postLens(t, r, `{"name":"eng","write":"alpha"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create: got %d, want 503", rec.Code)
	}
	// delete
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/lenses/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("delete: got %d, want 503", rec.Code)
	}
}
