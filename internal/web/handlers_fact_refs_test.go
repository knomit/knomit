package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ref gate must hold on the REST write paths, not just on MCP.
//
// Before this, POST /facts and PUT /facts assigned the client's refs verbatim:
// the "a stored local fact ref resolves" invariant held for knomit_learn and
// was bypassable by pointing a different API at the same branch. Everything
// downstream — the edge builder, the provenance walk, the UI's fact/broken
// split — was already written assuming the state could not exist.
//
// Both handlers route through the same refs.Gate as knomit_learn, so these
// assert the shared behaviour reached them, not a second implementation of it.

func TestFactCreate_RejectsUnresolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"} // empty corpus: nothing resolves
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	body := `{"title":"T","body":"b","type":"observation","domain":["ai"],
	          "refs":["know/ai/ml/nope.md"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "know/ai/ml/nope.md") {
		t.Errorf("the problem detail must name the offending ref: %s", rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("a rejected create must never reach git, got %d writes", writer.writeCalls)
	}
}

// The positive control, so the test above is not passing because refs are
// rejected wholesale: a ref that DOES resolve is accepted.
func TestFactCreate_AcceptsResolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{
		writeHash: "abc123",
		existing:  map[string]bool{"know/ai/ml/target.md": true},
	}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	body := `{"title":"T","body":"b","type":"observation","domain":["ai"],
	          "refs":["know/ai/ml/target.md"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 1 {
		t.Errorf("write calls: got %d, want 1", writer.writeCalls)
	}

	// The response view classifies the ref against the same resolver, so a ref
	// the gate accepted must not come back "broken".
	var view struct {
		Refs []RefView `json:"refs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(view.Refs) != 1 {
		t.Fatalf("refs: got %d, want 1", len(view.Refs))
	}
	if view.Refs[0].Path != "know/ai/ml/target.md" {
		t.Errorf("path: got %q, want the repo-relative path", view.Refs[0].Path)
	}
}

// PUT is gated for the same reason knomit_update is: refs replace wholesale, so
// a create-only gate is bypassed by writing a fact clean and then updating its
// refs to garbage.
func TestFactUpdate_RejectsUnresolvableLocalRef(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: writer},
	}

	content := "---\ntitle: T\ntype: observation\ndomain: [ai]\nconfidence: 0.8\nsources: 1\n" +
		"refs:\n  - know/ai/ml/nope.md\n---\n\n# T\n\nbody\n"
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/main/facts/know/ai/ml/xyz99999.md", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	s.NewAPIRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("a rejected update must never reach git, got %d writes", writer.writeCalls)
	}
}
