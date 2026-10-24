package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postLensRename POSTs a rename request verbatim — renames never carry a
// uid-spelled member, so there is nothing for lensReq to translate.
func postLensRename(t *testing.T, r http.Handler, lens, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/lenses/"+lens+"/rename", strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// TestHandleLensRename_InvalidNameIs400 needs no seeded lens: RenameLens
// checks isValidRepoName(newName) before it even looks at oldName. It DOES
// need a started manager (real registry), though — the handler's own
// registry-unavailable check runs first and would otherwise answer 503
// before RenameLens is ever called.
//
// 400, not 422: a name-grammar failure is a fixed-grammar syntax check,
// matching repo create, lens create/patch, and archive/restore. 422 is
// reserved for cross-referential failures (unknown repo/branch reference,
// over-cap description) — a bad-characters name is not that.
func TestHandleLensRename_InvalidNameIs400(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()
	rec := postLensRename(t, r, "eng", `{"name":"Has Capitals"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_MissingNameIs400(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()
	rec := postLensRename(t, r, "eng", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_BadJSONIs400(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()
	rec := postLensRename(t, r, "eng", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleLensRename_UnknownLensIs404 pins the not-found answer: unlike
// the repo route, nothing resolves {lens} ahead of this handler (see the
// ErrLensNotFound arm of lensRenameErrStatus), so an unknown oldName is a
// plain 404, not the repo route's 409.
func TestHandleLensRename_UnknownLensIs404(t *testing.T) {
	m, _ := newTestLensManager(t)
	r := (&Server{Manager: m}).NewAPIRouter()
	rec := postLensRename(t, r, "ghost", `{"name":"beta"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_NameTakenByLensIs409(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, m, r) // "eng", write alpha, read beta
	if rec := postLens(t, m, r, `{"name":"other","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed other: %d body=%s", rec.Code, rec.Body.String())
	}

	rec := postLensRename(t, r, "eng", `{"name":"other"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_NameTakenByRepoIs409(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, m, r) // "eng", write alpha, read beta

	rec := postLensRename(t, r, "eng", `{"name":"beta"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleLensRename_SameNameIsNoop covers RenameLens's documented
// successful no-op: renaming a lens to its own current name is not an error.
func TestHandleLensRename_SameNameIsNoop(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, m, r)

	rec := postLensRename(t, r, "eng", `{"name":"eng"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_SelfLinkPointsAtTheNewName(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, m, r)

	rec := postLensRename(t, r, "eng", `{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Name != "renamed" {
		t.Errorf("name: got %q, want %q", body.Name, "renamed")
	}
	if !strings.Contains(body.Links["self"].Href, "/lenses/renamed") {
		t.Errorf("self link: got %q, must address the lens by its new name", body.Links["self"].Href)
	}

	// The old name must no longer resolve.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get old name: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleLensRename_RegistryNil503(t *testing.T) {
	m := newTestManagerWithRepos(t) // never Start()-ed: nil lens registry
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := postLensRename(t, r, "eng", `{"name":"renamed"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}
