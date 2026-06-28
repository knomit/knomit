package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createViaAPI POSTs a preset-create and drains the NDJSON stream.
func createViaAPI(t *testing.T, r http.Handler, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"`+name+`","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create %s: status %d body %s", name, rec.Code, rec.Body.String())
	}
}

func TestArchiveLifecycle_HTTP(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")

	// DELETE /repos/work → archive
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/work", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status %d body %s", rec.Code, rec.Body.String())
	}
	var info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode archive: %v", err)
	}
	if info.ID == "" || info.Name != "work" {
		t.Fatalf("bad archive info: %+v", info)
	}
	if s.Manager.Get("work") != nil {
		t.Fatal("work should be unregistered after archive")
	}

	// GET /archived → contains it
	grec := httptest.NewRecorder()
	r.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if grec.Code != http.StatusOK {
		t.Fatalf("list status %d", grec.Code)
	}
	if !strings.Contains(grec.Body.String(), info.ID) {
		t.Fatalf("archived list missing id: %s", grec.Body.String())
	}

	// POST restore
	rrec := httptest.NewRecorder()
	r.ServeHTTP(rrec, httptest.NewRequest(http.MethodPost, "/archived/"+info.ID+"/restore",
		strings.NewReader(`{}`)))
	if rrec.Code != http.StatusOK {
		t.Fatalf("restore status %d body %s", rrec.Code, rrec.Body.String())
	}
	if s.Manager.Get("work") == nil {
		t.Fatal("work should be active after restore")
	}
}

func TestArchiveDefault_Conflict(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/trunk", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}

func TestPurge_HTTP(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")
	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, "/repos/work", nil))
	var info struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(drec.Body.Bytes(), &info)

	prec := httptest.NewRecorder()
	r.ServeHTTP(prec, httptest.NewRequest(http.MethodDelete, "/archived/"+info.ID, nil))
	if prec.Code != http.StatusNoContent {
		t.Fatalf("purge status %d, want 204", prec.Code)
	}
}
