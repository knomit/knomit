package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/version"
	"knomit/internal/web/hal"
)

func TestHandleVersion_ReturnsBuildVersion(t *testing.T) {
	orig, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = orig, origCommit })
	version.Version, version.Commit = "0.5.0", "2a7ae9d"

	b := hal.URLBuilder{Base: APIBase}
	req := httptest.NewRequest(http.MethodGet, APIBase+"/version", nil)
	rec := httptest.NewRecorder()

	handleVersion(b, false).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != hal.ContentType {
		t.Errorf("content-type = %q, want %q", ct, hal.ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["version"] != "0.5.0" {
		t.Errorf("version = %v, want 0.5.0", body["version"])
	}
	if body["commit"] != "2a7ae9d" {
		t.Errorf("commit = %v, want 2a7ae9d", body["commit"])
	}
	if body["full"] != "0.5.0.2a7ae9d" {
		t.Errorf("full = %v, want 0.5.0.2a7ae9d", body["full"])
	}
	links, ok := body["_links"].(map[string]any)
	if !ok {
		t.Fatalf("_links missing or wrong type: %v", body["_links"])
	}
	self, ok := links["self"].(map[string]any)
	if !ok || self["href"] != APIBase+"/version" {
		t.Errorf("self link = %v, want href %s", links["self"], APIBase+"/version")
	}
}

func TestHandleVersion_ExposesReadOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	handleVersion(hal.URLBuilder{Base: "/api/v1"}, true)(rec, httptest.NewRequest("GET", "/api/v1/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["read_only"] != true {
		t.Fatalf("read_only = %v, want true", body["read_only"])
	}
}
