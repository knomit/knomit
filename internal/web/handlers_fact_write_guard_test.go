package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refusedByGuard drives PUT and DELETE at path and asserts BOTH are refused
// with 400 and that neither ever reached the writer. The pair matters: the two
// handlers carry the same rule in two places, and a fix applied to one only is
// the exact shape of bug this file exists to catch.
func refusedByGuard(t *testing.T, path string) {
	t.Helper()
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{factWriter: writer},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/"+path,
		strings.NewReader(`{"content":"`+testFactContent+`"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT %s: status %d, want 400, body=%s", path, rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("PUT %s: writer.Write called %d times; it must never reach git", path, writer.writeCalls)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete,
		"/repos/alpha/branches/agent:test/facts/"+path, nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE %s: status %d, want 400, body=%s", path, rec.Code, rec.Body.String())
	}
	if writer.deleteCalls != 0 {
		t.Errorf("DELETE %s: writer.Delete called %d times; it must never reach git", path, writer.deleteCalls)
	}
}

// TestFactEndpoints_CannotShadowAServerOwnedFileWithADirectory is the endpoint
// half of the dotless-area rule. The depth rule protects .knomit/ontology.yaml
// by NAME; nothing stopped that name being reused as a DIRECTORY. store's
// buildTree drops an existing entry of the same name whatever its mode, so
// PUT …/facts/.knomit/ontology.yaml/x.md replaces the ontology BLOB with a
// tree — verified end-to-end against a real store: the write succeeded and the
// next read failed with "blob: object not found", after which loadOntology
// falls through to the embedded default and every later fact is validated
// against the wrong taxonomy.
func TestFactEndpoints_CannotShadowAServerOwnedFileWithADirectory(t *testing.T) {
	for _, path := range []string{
		".knomit/ontology.yaml/x.md",
		".knomit/ontology.yaml/deeper/x.md",
		".knomit/foo.bar/x.md",
	} {
		t.Run(path, func(t *testing.T) { refusedByGuard(t, path) })
	}
}
