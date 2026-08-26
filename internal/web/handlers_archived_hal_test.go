package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// createViaAPI POSTs a preset-create and polls the returned create job to
// completion, so callers can treat it as "the repo now exists".
//
// The polling is not ceremony: POST /repos answers 202 and returns before the
// repo is registered (issue #67 — the response no longer holds the work), so a
// test that went straight from create to DELETE would race the create it just
// asked for. Every consumer of an async create has to wait somewhere; for
// tests, here is that somewhere.
func createViaAPI(t *testing.T, r http.Handler, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"`+name+`","mode":"preset","ontology_preset":"default"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create %s: status %d body %s", name, rec.Code, rec.Body.String())
	}
	awaitCreate(t, r, rec)
}

// awaitCreate polls the create job named by a 202 response until it reaches a
// terminal state, failing the test on a failed create or on a job that never
// finishes.
func awaitCreate(t *testing.T, r http.Handler, accepted *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(accepted.Body.Bytes(), &body); err != nil {
		t.Fatalf("202 body is not JSON: %v (%s)", err, accepted.Body.String())
	}
	// FOLLOW THE LINK the server gave us rather than rebuilding the path.
	// Hardcoding it here would make every test that creates a repo fail
	// whenever the route moves, which drowns the ONE test that is actually
	// about where the route lives (TestReposNamedCreatesStaysReachable) in
	// noise from tests that do not care.
	self := accepted.Header().Get("Location")
	if self == "" {
		t.Fatalf("202 carries no Location header: %s", accepted.Body.String())
	}
	self = strings.TrimPrefix(self, APIBase)
	if id, _ := body["create_id"].(string); id == "" {
		t.Fatalf("202 body carries no create_id: %s", accepted.Body.String())
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, self, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("poll %s: status %d body %s", self, rec.Code, rec.Body.String())
		}
		var st map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("poll body is not JSON: %v (%s)", err, rec.Body.String())
		}
		switch st["state"] {
		case "done":
			return st
		case "failed":
			t.Fatalf("create failed: %v", st["error"])
		}
		if time.Now().After(deadline) {
			t.Fatalf("create never reached a terminal state: %v", st)
		}
		time.Sleep(5 * time.Millisecond)
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

// newRealManagerInHome is newRealManager with the home directory visible, so a
// test can reach control.db directly.
func newRealManagerInHome(t *testing.T, home string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// Restoring an archived repo whose knowledge base another ACTIVE repo already
// holds is a user-correctable conflict (archive the holder, then restore), and
// it must be refused rather than half-done.
//
// Two things used to go wrong, and this test covers both.
//
// Restore never recorded repo_id. It leaned on SetState tripping
// repos_active_repo_id — but that index is WHERE state='active' AND repo_id IS
// NOT NULL, so an archived row with a NULL repo_id (exactly what
// migrate-registry writes for a repo whose HEAD it could not resolve, and what
// any repo archived before repo_id existed has) flips to active unchallenged
// and STAYS null. Two live copies of one knowledge base then both write
// agent/<host> and clobber each other on push.
//
// And ErrRepoAlreadyRegistered had no arm in archiveErrStatus, so even when the
// registry did refuse, the user got 500 "Operation failed".
func TestRestore_ConflictingKnowledgeBaseIs409(t *testing.T) {
	home := t.TempDir()
	s := &Server{Manager: newRealManagerInHome(t, home)}
	r := s.NewAPIRouter()
	createViaAPI(t, r, "keeper")
	createViaAPI(t, r, "work")

	drec := httptest.NewRecorder()
	r.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, "/repos/work", nil))
	if drec.Code != http.StatusOK {
		t.Fatalf("archive status %d body %s", drec.Code, drec.Body.String())
	}
	var archived struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(drec.Body.Bytes(), &archived); err != nil {
		t.Fatalf("decode archive: %v", err)
	}

	// Hand the archived repo's knowledge base to the active one, and blank the
	// archived row's repo_id — the legacy/migrated shape the index cannot see.
	db, err := sql.Open("sqlite3", filepath.Join(home, "control.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open control.db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(
		`UPDATE repos SET repo_id = (SELECT repo_id FROM repos WHERE uid = ?) WHERE name = 'keeper'`,
		archived.ID); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}
	if _, err := db.Exec(`UPDATE repos SET repo_id = NULL WHERE uid = ?`, archived.ID); err != nil {
		t.Fatalf("blank archived repo_id: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close control.db: %v", err)
	}

	rrec := httptest.NewRecorder()
	r.ServeHTTP(rrec, httptest.NewRequest(http.MethodPost, "/archived/"+archived.ID+"/restore",
		strings.NewReader(`{}`)))
	if rrec.Code != http.StatusConflict {
		t.Fatalf("restore into a taken knowledge base: status %d, want 409; body=%s",
			rrec.Code, rrec.Body.String())
	}
	if s.Manager.Get("work") != nil {
		t.Fatal("a refused restore must not leave a second live copy registered")
	}

	// And it is still archived, so the operator can retry after archiving the
	// holder — a rollback that only half-happened would strand it.
	grec := httptest.NewRecorder()
	r.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/archived", nil))
	if !strings.Contains(grec.Body.String(), archived.ID) {
		t.Fatalf("refused restore lost the archive row: %s", grec.Body.String())
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
