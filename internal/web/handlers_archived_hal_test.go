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

// TestArchiveLastRepo_OK pins that the API lets you archive the only repo you
// have. There is no privileged repo and no last-repo guard, so this is a plain
// 200 rather than the 409 the removed guards used to produce.
func TestArchiveLastRepo_OK(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "only")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/only", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := s.Manager.Names(); len(got) != 0 {
		t.Fatalf("repos after archiving the last one = %v, want none", got)
	}
}

// The archived listing reports each archive's on-disk size. Archived databases
// no longer live under a human-readable filename — there is no directory to
// `ls` — so without this the disk a purge would reclaim is invisible from every
// surface the user has. The archive response carries it too: that is the last
// thing the caller is told about the repo, and it must not be the one shape
// that omits it.
func TestArchived_ReportsSizeBytes(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "work")

	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, "/repos/work", nil))
	if drec.Code != http.StatusOK {
		t.Fatalf("archive status %d body %s", drec.Code, drec.Body.String())
	}
	var archived struct {
		ID        string `json:"id"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(drec.Body.Bytes(), &archived); err != nil {
		t.Fatalf("decode archive: %v", err)
	}
	if archived.SizeBytes <= 0 {
		t.Errorf("archive response sizeBytes: got %d, want the database's size; body=%s",
			archived.SizeBytes, drec.Body.String())
	}

	grec := httptest.NewRecorder()
	r.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if grec.Code != http.StatusOK {
		t.Fatalf("list status %d", grec.Code)
	}
	var list struct {
		Embedded struct {
			Archived []struct {
				ID        string `json:"id"`
				SizeBytes int64  `json:"sizeBytes"`
			} `json:"archived"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(grec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, grec.Body.String())
	}
	if len(list.Embedded.Archived) != 1 {
		t.Fatalf("archived: got %d items, want 1; body=%s", len(list.Embedded.Archived), grec.Body.String())
	}
	got := list.Embedded.Archived[0]
	if got.ID != archived.ID {
		t.Fatalf("archived id: got %q, want %q", got.ID, archived.ID)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("listing sizeBytes: got %d, want the database's size; body=%s",
			got.SizeBytes, grec.Body.String())
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
