package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/repos"
)

// TestPostRepoCreateCancel_DoneJobIs202AndTheRepoIsGone pins the contract the
// wizard relies on: cancelling a create that already landed answers 202 with
// the job snapshot already reading `cancelled`, carries no error and no repo
// link, and the repo is neither live NOR archived afterwards.
func TestPostRepoCreateCancel_DoneJobIs202AndTheRepoIsGone(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	id := startCreate(t, r, "work")
	final := awaitCreateID(t, r, id)
	if final["state"] != "done" {
		t.Fatalf("precondition: state = %v, want done", final["state"])
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repo-creates/"+id+":cancel", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad cancel body: %v", err)
	}
	if body["state"] != "cancelled" {
		t.Fatalf("state = %v, want cancelled: %v", body["state"], body)
	}
	if _, ok := body["error"]; ok {
		t.Fatalf("a cancelled job must not carry an error: %v", body)
	}
	if _, ok := body["repo"]; ok {
		t.Fatalf("a cancelled job must not offer a repo link: %v", body)
	}
	if body["create_id"] != id {
		t.Fatalf("cancel body is not the job's own snapshot: %v", body)
	}

	// The poll agrees with the 202.
	poll := httptest.NewRecorder()
	r.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/repo-creates/"+id, nil))
	var polled map[string]any
	if err := json.Unmarshal(poll.Body.Bytes(), &polled); err != nil {
		t.Fatalf("bad poll body: %v", err)
	}
	if polled["state"] != "cancelled" {
		t.Fatalf("poll state = %v, want cancelled", polled["state"])
	}

	// Gone, not archived.
	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/repos/work", nil))
	if get.Code != http.StatusNotFound {
		t.Fatalf("GET /repos/work = %d, want 404 after cancel", get.Code)
	}
	arch := httptest.NewRecorder()
	r.ServeHTTP(arch, httptest.NewRequest(http.MethodGet, "/archived", nil))
	var archived map[string]any
	if err := json.Unmarshal(arch.Body.Bytes(), &archived); err != nil {
		t.Fatalf("bad archived body: %v", err)
	}
	if n, _ := archived["count"].(float64); n != 0 {
		t.Fatalf("cancel must not archive: %s", arch.Body.String())
	}
	if s.Manager.Get("work") != nil {
		t.Fatal("repo still registered after cancel")
	}

	// A second cancel has nothing to undo.
	again := httptest.NewRecorder()
	r.ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/repo-creates/"+id+":cancel", nil))
	if again.Code != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409, body=%s", again.Code, again.Body.String())
	}
}

// TestPostRepoCreateCancel_RunningJobEndsCancelled: cancelling right after the
// 202 must end the job in `cancelled` with nothing registered, whichever step
// the cancel lands on.
func TestPostRepoCreateCancel_RunningJobEndsCancelled(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	id := startCreate(t, r, "work")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repo-creates/"+id+":cancel", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202, body=%s", rec.Code, rec.Body.String())
	}
	final := awaitCreateID(t, r, id)
	if final["state"] != "cancelled" {
		t.Fatalf("final state = %v, want cancelled: %v", final["state"], final)
	}
	if s.Manager.Get("work") != nil {
		t.Fatal("repo registered despite cancel")
	}
}

// TestPostRepoCreateCancel_UnknownIs404 — unknown and expired are one answer,
// as on every other route of this collection.
func TestPostRepoCreateCancel_UnknownIs404(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repo-creates/nope:cancel", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestPostRepoCreateCancel_FailedJobIs409: a failed create already rolled
// itself back; cancel has nothing to do and says so rather than pretending.
func TestPostRepoCreateCancel_FailedJobIs409(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:           config.Config{Home: t.TempDir()},
		AgentBranch:   "machine/test",
		CreateTimeout: time.Nanosecond, // expired before the worker runs
	})
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	id := startCreate(t, r, "work")
	final := awaitCreateID(t, r, id)
	if final["state"] != "failed" {
		t.Fatalf("precondition: state = %v, want failed", final["state"])
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repo-creates/"+id+":cancel", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
}
