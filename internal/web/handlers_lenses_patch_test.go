package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/repos"
)

// patchLens issues a PATCH /lenses/{name} with the given raw JSON body.
func patchLens(t *testing.T, r http.Handler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/lenses/"+name, bytes.NewBufferString(body))
	r.ServeHTTP(rec, req)
	return rec
}

// seedEng creates the "eng" lens (write alpha, read beta) and fails the test if
// creation does not return 201.
func seedEng(t *testing.T, r http.Handler) {
	t.Helper()
	if rec := postLens(t, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_ReplaceReads(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta", "gamma")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"reads":[{"repo":"gamma"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := readRepos(body)
	// beta replaced wholesale by gamma; write repo alpha folded in.
	if len(got) != 2 || got["alpha"] == false || got["gamma"] == false || got["beta"] {
		t.Errorf("reads after replace: got %v, want {alpha,gamma}", got)
	}
}

func TestHandleHALLensPatch_ChangeWrite(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"write":"beta"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Write != "beta" {
		t.Errorf("write: got %q, want beta", body.Write)
	}
}

func TestHandleHALLensPatch_DescriptionOnlyKeepsMounts(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"description":"just docs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body lensViewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Description != "just docs" {
		t.Errorf("description: got %q", body.Description)
	}
	if body.Write != "alpha" {
		t.Errorf("write must be untouched: got %q", body.Write)
	}
	got := readRepos(body)
	if len(got) != 2 || !got["alpha"] || !got["beta"] {
		t.Errorf("mounts must be untouched: got %v, want {alpha,beta}", got)
	}
}

func TestHandleHALLensPatch_UnknownLens404(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha")
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := patchLens(t, r, "missing", `{"description":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q", got)
	}
}

func TestHandleHALLensPatch_UnknownMember422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"reads":[{"repo":"ghost"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_Replica409(t *testing.T) {
	m, home := newTestLensManager(t, "alpha", "beta")
	cloneRepo(t, m, home, "alpha", "alpha_clone")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"reads":[{"repo":"alpha_clone"}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_BadBranchPin422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"reads":[{"repo":"beta","branch":"nope"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_EmptyWrite400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{"write":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_DescriptionTooLong422(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	body, err := json.Marshal(map[string]any{"description": strings.Repeat("x", 4097)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := patchLens(t, r, "eng", string(body))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_BadJSON400(t *testing.T) {
	m, _ := newTestLensManager(t, "alpha", "beta")
	r := (&Server{Manager: m}).NewAPIRouter()
	seedEng(t, r)

	rec := patchLens(t, r, "eng", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALLensPatch_RegistryNil503(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	r := (&Server{Manager: m}).NewAPIRouter()

	rec := patchLens(t, r, "eng", `{"description":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("patch: got %d, want 503", rec.Code)
	}
}

// readRepos flattens a lens view body's read mounts into a name→present set.
func readRepos(b lensViewBody) map[string]bool {
	out := map[string]bool{}
	for _, r := range b.Reads {
		out[r.Repo] = true
	}
	return out
}
