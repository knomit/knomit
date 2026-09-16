package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// GET /repo-creates lists every job the manager holds, newest first, in the
// SAME body shape the single-job resource serves — so a client parses one
// shape, and a row in the list is a row it can already draw.
func TestGetRepoCreates_ListsNewestFirst(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	first := startCreate(t, r, "alpha")
	second := startCreate(t, r, "beta")
	awaitCreateID(t, r, first)
	awaitCreateID(t, r, second)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	creates := embeddedCreates(t, rec.Body.Bytes())
	if len(creates) != 2 {
		t.Fatalf("want 2 creates, got %d: %s", len(creates), rec.Body.String())
	}
	if creates[0]["create_id"] != second || creates[1]["create_id"] != first {
		t.Fatalf("want newest first (%s then %s), got %v / %v",
			second, first, creates[0]["create_id"], creates[1]["create_id"])
	}
	// The row carries everything the poll resource does, including the new
	// fields a client draws a bar from.
	row := creates[0]
	for _, key := range []string{"create_id", "name", "mode", "state", "step", "message", "pct", "phase", "indeterminate", "_links"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("row is missing %q: %v", key, row)
		}
	}
	if row["state"] != "done" {
		t.Fatalf("state = %v, want done", row["state"])
	}
	// "done" now means INDEXED, and the row says which.
	if row["index_state"] != "ready" {
		t.Fatalf("index_state = %v, want ready: %v", row["index_state"], row)
	}
	if row["phase"] != "done" {
		t.Fatalf("phase = %v, want done", row["phase"])
	}
}

// An empty list is an empty COLLECTION, not a 404 and not a null: a client
// polling this endpoint from boot must not have to special-case "no creates".
func TestGetRepoCreates_EmptyIsACollection(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := embeddedCreates(t, rec.Body.Bytes()); len(got) != 0 {
		t.Fatalf("want an empty collection, got %v", got)
	}
	if !strings.Contains(rec.Body.String(), `"creates"`) {
		t.Fatalf("the embedded key must be present even when empty: %s", rec.Body.String())
	}
}

// DELETE forgets a FINISHED job (204) and the row is gone; the id then reads
// as unknown, which is the same answer an expired one gives.
func TestDeleteRepoCreate_DismissesAFinishedJob(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	r := s.NewAPIRouter()

	id := startCreate(t, r, "gamma")
	awaitCreateID(t, r, id)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repo-creates/"+id, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}

	list := httptest.NewRecorder()
	r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	if got := embeddedCreates(t, list.Body.Bytes()); len(got) != 0 {
		t.Fatalf("dismissed job is still listed: %v", got)
	}

	poll := httptest.NewRecorder()
	r.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/repo-creates/"+id, nil))
	if poll.Code != http.StatusNotFound {
		t.Fatalf("poll after dismiss = %d, want 404", poll.Code)
	}

	// The repo itself is untouched: dismissing a row forgets the JOB, never
	// the thing the job made.
	if s.Manager.Get("gamma") == nil {
		t.Fatal("dismissing a create must not remove the repo it created")
	}
}

func TestDeleteRepoCreate_UnknownIDIs404(t *testing.T) {
	s := &Server{Manager: newRealManager(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repo-creates/nosuchjob", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// awaitCreateID polls one job to a terminal state and returns its final body.
// The list tests need the id (they assert on ordering), so they cannot reuse
// awaitCreate, which follows a 202's Location header.
func awaitCreateID(t *testing.T, r http.Handler, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo-creates/"+id, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("poll %s: status %d body %s", id, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad poll body: %v", err)
		}
		if st, _ := body["state"].(string); st != "running" {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("create %s never finished", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startCreate posts a preset create and returns its job id.
func startCreate(t *testing.T, r http.Handler, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"`+name+`","mode":"preset","ontology_preset":"default"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create %q: status = %d, body=%s", name, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad 202 body: %v", err)
	}
	id, _ := body["create_id"].(string)
	if id == "" {
		t.Fatalf("no create_id in %v", body)
	}
	return id
}

func embeddedCreates(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Creates []map[string]any `json:"creates"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("bad collection body %q: %v", raw, err)
	}
	if body.Count != len(body.Embedded.Creates) {
		t.Fatalf("count %d disagrees with %d embedded rows", body.Count, len(body.Embedded.Creates))
	}
	return body.Embedded.Creates
}
