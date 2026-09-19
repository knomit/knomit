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
	// `cancelling`, NOT `cancelled`: the handler returns before the delete it
	// authorised has run. A 202 that already claimed `cancelled` would be the
	// response promising an outcome it has not observed.
	if body["state"] != "cancelling" {
		t.Fatalf("state = %v, want cancelling: %v", body["state"], body)
	}
	if body["create_id"] != id {
		t.Fatalf("cancel body is not the job's own snapshot: %v", body)
	}

	// A SECOND cancel while the first is still being honoured is 202 again,
	// not 409. The request is idempotent, and a control whose own label reads
	// "Cancelling…" must not fail when pressed twice.
	dup := httptest.NewRecorder()
	r.ServeHTTP(dup, httptest.NewRequest(http.MethodPost, "/repo-creates/"+id+":cancel", nil))
	if dup.Code != http.StatusAccepted {
		t.Fatalf("second cancel while cancelling = %d, want 202, body=%s", dup.Code, dup.Body.String())
	}

	// The poll carries it to the terminal state.
	polled := awaitCreateID(t, r, id)
	if polled["state"] != "cancelled" {
		t.Fatalf("poll state = %v, want cancelled", polled["state"])
	}
	if _, ok := polled["error"]; ok {
		t.Fatalf("a cancelled job must not carry an error: %v", polled)
	}
	if _, ok := polled["repo"]; ok {
		t.Fatalf("a cancelled job must not offer a repo link: %v", polled)
	}

	// ...and it is GONE from the collection, while still readable by id above.
	// A repository list has nothing left to say about a create whose outcome
	// is the one the user asked for.
	list := httptest.NewRecorder()
	r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	for _, c := range embeddedCreates(t, list.Body.Bytes()) {
		if c["create_id"] == id {
			t.Fatalf("a cancelled job must not be listed: %s", list.Body.String())
		}
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
	var accepted map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("bad cancel body: %v", err)
	}
	// Never still `running` once the request is recorded: that stale report is
	// what made the UI look frozen after the click.
	if st := accepted["state"]; st != "cancelling" && st != "cancelled" {
		t.Fatalf("state = %v, want cancelling (or already cancelled): %v", st, accepted)
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
