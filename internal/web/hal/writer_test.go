package hal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteHAL_Success_SetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	body := map[string]any{"hello": "world", "_links": LinkMap{"self": {Href: "/a"}}}
	WriteHAL(rec, http.StatusOK, body)

	if got := rec.Header().Get("Content-Type"); got != "application/hal+json" {
		t.Errorf("content-type: got %q, want application/hal+json", got)
	}
	if got := rec.Code; got != http.StatusOK {
		t.Errorf("status: got %d, want 200", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["hello"] != "world" {
		t.Errorf("body: %+v", decoded)
	}
}

func TestWriteHAL_Created_WithLocation(t *testing.T) {
	rec := httptest.NewRecorder()
	body := map[string]any{"id": "xyz"}
	WriteHALCreated(rec, "/api/v1/repos/alpha/branches/agent:test/facts/know/x.md", body)

	if got := rec.Code; got != http.StatusCreated {
		t.Errorf("status: got %d, want 201", got)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/repos/alpha/branches/agent:test/facts/know/x.md" {
		t.Errorf("location: got %q", got)
	}
	if got := rec.Header().Get("Content-Location"); got != "/api/v1/repos/alpha/branches/agent:test/facts/know/x.md" {
		t.Errorf("content-location: got %q", got)
	}
}
