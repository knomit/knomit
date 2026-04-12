package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServerWithJobRegistry(t *testing.T, repos ...string) *Server {
	t.Helper()
	return &Server{
		Manager:     newTestManagerWithRepos(t, repos...),
		JobRegistry: NewJobRegistry(),
	}
}

func TestHandleListJobs_EmptyRegistry_Returns200(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	r := s.NewAPIRouter()

	for _, kind := range []string{"synthesis-runs", "index-rebuilds"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/repos/alpha/branches/main/"+kind, nil)
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200, body=%s", kind, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Count    int              `json:"count"`
			Embedded map[string][]any `json:"_embedded"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: unmarshal: %v", kind, err)
		}
		if body.Count != 0 {
			t.Errorf("%s: count: got %d, want 0", kind, body.Count)
		}
	}
}

func TestHandleListJobs_WithEntries_ReturnsList(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	s.JobRegistry.Register("synth-1", "synthesis-run")
	s.JobRegistry.Register("synth-2", "synthesis-run")
	s.JobRegistry.Register("rebuild-1", "index-rebuild")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/synthesis-runs", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: got %d, want 2 (synthesis-run only)", body.Count)
	}
}

func TestHandleGetJob_Found_Returns200(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	s.JobRegistry.Register("synth-1", "synthesis-run")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/synthesis-runs/synth-1", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ID != "synth-1" {
		t.Errorf("id: got %q, want synth-1", body.ID)
	}
	if body.State != "running" {
		t.Errorf("state: got %q, want running", body.State)
	}
}

func TestHandleGetJob_NotFound_Returns404(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/synthesis-runs/nope", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleDeleteJob_DoneJob_Returns204(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	s.JobRegistry.Register("rebuild-1", "index-rebuild")
	s.JobRegistry.Complete("rebuild-1", "done", "")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/alpha/branches/main/index-rebuilds/rebuild-1", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: %d, want 204", rec.Code)
	}
	// Verify it was removed.
	if s.JobRegistry.Get("rebuild-1") != nil {
		t.Error("job should have been deleted from registry")
	}
}

func TestHandleDeleteJob_RunningJob_Returns409(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	s.JobRegistry.Register("rebuild-1", "index-rebuild")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/alpha/branches/main/index-rebuilds/rebuild-1", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status: %d, want 409 (running job)", rec.Code)
	}
}

func TestHandleDeleteJob_NotFound_Returns204(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/alpha/branches/main/synthesis-runs/ghost", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: %d, want 204 (idempotent)", rec.Code)
	}
}

func TestHandleJobEvents_UnknownJob_Returns404(t *testing.T) {
	s := newServerWithJobRegistry(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/main/synthesis-runs/nope/events", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

func TestHandleJobEvents_UnknownRepo_Returns404(t *testing.T) {
	s := newServerWithJobRegistry(t)
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/synthesis-runs/x/events", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}
