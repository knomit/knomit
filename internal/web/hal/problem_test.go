package hal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProblem_Marshal_RequiredFields(t *testing.T) {
	p := Problem{
		Type:     "about:blank",
		Title:    "Fact not found",
		Status:   404,
		Detail:   `no fact at path "know/nope.md"`,
		Instance: "/api/v1-new/repos/alpha/branches/agent:test/facts/know/nope.md",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"type":"about:blank"`,
		`"title":"Fact not found"`,
		`"status":404`,
		`"detail":"no fact at path \"know/nope.md\""`,
		`"instance":"/api/v1-new/repos/alpha/branches/agent:test/facts/know/nope.md"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in %s", want, b)
		}
	}
}

func TestWriteProblem_SetsContentTypeAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, http.StatusNotFound, "Fact not found", "no fact at path \"know/nope.md\"", "/foo")

	if got := rec.Code; got != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Status != 404 || p.Title != "Fact not found" {
		t.Errorf("decoded: %+v", p)
	}
}
