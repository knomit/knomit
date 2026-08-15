package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/fact"
)

func TestValidateOntology_OKReturnsSummary(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	body := "id: x\nname: X\ntopics:\n  alpha:\n    description: d\n"
	req := httptest.NewRequest(http.MethodPost, "/ontologies:validate", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK        bool     `json:"ok"`
		ID        string   `json:"id"`
		Topics    []string `json:"topics"`
		RuleCount int      `json:"rule_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.ID != "x" || len(got.Topics) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// An invalid ontology is a 200 carrying ok:false — not an HTTP error. The
// client renders diagnostics inline; a 4xx would make "you typed a bad key"
// indistinguishable from "the request was malformed".
func TestValidateOntology_InvalidReturns200WithDiagnostics(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	body := "id: x\nname: X\ntopics:\n  Bad Key:\n    description: d\n"
	req := httptest.NewRequest(http.MethodPost, "/ontologies:validate", strings.NewReader(body))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		OK          bool `json:"ok"`
		Diagnostics []struct {
			Line    int    `json:"line"`
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.OK {
		t.Fatalf("ok = true, want false")
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Line != 4 {
		t.Fatalf("diagnostics = %+v", got.Diagnostics)
	}
}

func TestValidateOntology_OversizeRejected(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ontologies:validate",
		strings.NewReader(strings.Repeat("x", MaxOntologyBytes+1)))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestOntologyPresets_ListsBothPresets(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ontologies/presets", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Presets []struct {
			Name   string   `json:"name"`
			Topics []string `json:"topics"`
		} `json:"presets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Presets) != 2 {
		t.Fatalf("presets = %d, want 2 (default, code)", len(got.Presets))
	}
	for _, p := range got.Presets {
		if len(p.Topics) == 0 {
			t.Errorf("preset %q has no topics", p.Name)
		}
	}
}

func TestOntologyPresetYAML_RoundTripsThroughParser(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ontologies/presets/code", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The editor seeds itself from this, so it must be valid input to the
	// validator the editor then calls.
	if _, diags := factValidate(rec.Body.Bytes()); len(diags) != 0 {
		t.Fatalf("served preset does not validate: %+v", diags)
	}
}

func TestOntologyPresetYAML_UnknownIs404(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ontologies/presets/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOntologySchema_ServesFields(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ontologies/schema", nil))

	var got struct {
		Fields []struct {
			Struct string `json:"struct"`
			Field  string `json:"field"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Fields) != 11 {
		t.Fatalf("fields = %d, want 11", len(got.Fields))
	}
}

func factValidate(b []byte) (any, []fact.Diagnostic) {
	o, d := fact.ValidateOntologyYAML(b)
	return o, d
}
