package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
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
	// The handler's job is to SERVE the manager's order, not to invent one, so
	// that is what this asserts. It deliberately does NOT hardcode "second then
	// first": both creates are started back to back, and StartedAt is
	// time.Now(), whose resolution is the system clock's tick on Windows — so
	// the two routinely share an instant there and the manager's tiebreak, not
	// their start order, decides which comes first. Hardcoding the expectation
	// made this test pass on Linux and macOS and fail on Windows for a reason
	// that was never about the handler. The ordering CONTRACT is owned by
	// repos.CreateJobs and tested there against constructed timestamps
	// (TestCreateJobs_NewestFirstAndReapsExpired,
	// TestCreateJobs_SameInstantIsATotalOrder).
	want := make([]string, 0, 2)
	for _, st := range s.Manager.CreateJobs() {
		want = append(want, st.ID)
	}
	if creates[0]["create_id"] != want[0] || creates[1]["create_id"] != want[1] {
		t.Fatalf("handler reordered the manager's list: want %v, got %v / %v",
			want, creates[0]["create_id"], creates[1]["create_id"])
	}
	// Both creates are present, whatever order the tie resolved to.
	got := map[any]bool{creates[0]["create_id"]: true, creates[1]["create_id"]: true}
	if !got[first] || !got[second] {
		t.Fatalf("want both %s and %s listed, got %v", first, second, got)
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

// Dismissing a RUNNING create is 409, not 204 and not a cancel.
//
// The manager-level refusal is covered by TestDismissCreateJob; what this pins
// is the HTTP MAPPING, which is the part a client actually sees and the part
// that can silently regress on its own — deleting the ErrCreateRunning arm in
// createErrStatus' sibling switch drops a running job into the default 404 and
// every other test in this package stays green.
//
// A create that cannot be dismissed must not read as a create that does not
// exist: 404 tells a client the work is gone, and it is still running.
func TestDeleteRepoCreate_RunningIs409(t *testing.T) {
	m := newRealManager(t)
	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	// A create that will NOT finish while the assertion runs. A subscribe to a
	// URL nothing answers sits in its network timeout, which is far longer than
	// this test — no sleep, no poll, no race against a fast local create.
	job := m.StartCreate(repos.CreateSpec{
		Name: "slow", Mode: "subscribe",
		Origin: &repos.OriginSpec{URL: "http://127.0.0.1:1/never"},
	})
	t.Cleanup(func() { <-job.Done() })

	// Assert the precondition rather than assume it: if this create had already
	// finished, the 409 below would be testing the 404 path under another name.
	if st := job.Status(); st.State != repos.CreateRunning {
		t.Fatalf("fixture create is not running (state=%s err=%v); this test cannot pin 409", st.State, st.Err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repo-creates/"+job.ID(), nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "still running") {
		t.Fatalf("the refusal must say why: %s", rec.Body.String())
	}

	// The refused dismiss changed nothing: the job is still listed and still
	// pollable, which is the whole reason 409 rather than 404 is the answer.
	list := httptest.NewRecorder()
	r.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	found := false
	for _, c := range embeddedCreates(t, list.Body.Bytes()) {
		if c["create_id"] == job.ID() {
			found = true
		}
	}
	if !found {
		t.Fatalf("a refused dismiss must leave the job listed: %s", list.Body.String())
	}
}

// THE WIRE CONTRACT FOR pct: present when there is a percentage, ABSENT when
// there is not. Not zero — the job's pct is a latest value, so a zero here
// would run 5 → 0 → 70 across a create and send a monotonic client's bar
// backwards mid-transfer.
//
// This is the server half of the rule the UI honours; PendingCreateRow's
// vitest is the client half. The two are pinned separately on purpose: they
// are what a THIRD client would read, and a body that kept sending 40 while
// saying "indeterminate" would draw the incident's frozen bar in anything that
// ignored the flag.
func TestCreateStatusBody_OmitsPctWhenIndeterminate(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}

	transfer := createStatusBody(b, repos.CreateStatus{
		ID: "c1", Name: "kb", Mode: "subscribe", State: repos.CreateRunning,
		Step: "subscribe", Phase: repos.PhaseTransfer, Indeterminate: true,
		Message: "knomit: sent 3 MiB", Pct: 40,
	})
	if _, ok := transfer["pct"]; ok {
		t.Fatalf("an indeterminate status must carry NO pct, got %v", transfer["pct"])
	}
	if transfer["indeterminate"] != true {
		t.Fatalf("indeterminate must be true: %v", transfer)
	}

	// And the other half of the contract: a determinate status DOES carry it,
	// or "absent" would mean nothing.
	index := createStatusBody(b, repos.CreateStatus{
		ID: "c1", Name: "kb", Mode: "subscribe", State: repos.CreateRunning,
		Step: "index", Phase: repos.PhaseIndex, Message: "indexing 40/60", Pct: 97,
	})
	if index["pct"] != 97 {
		t.Fatalf("a determinate status must carry its pct, got %v", index["pct"])
	}
	if index["indeterminate"] != false {
		t.Fatalf("indeterminate must be false: %v", index)
	}
}

// The local-origin policy answers 400 "Origin not allowed" at POST /repos —
// the SAME status and title PUT /origin has always answered for the same
// refusal. One policy, one status, whichever door the request came through.
//
// Without the sentinel arm in createErrStatus this falls to the default and
// answers 500: a server error for a request the server understood perfectly
// and declined on policy, which tells a client to retry something that will
// never be allowed.
func TestPostRepos_LocalOriginOutsideTheRootIs400(t *testing.T) {
	// newRealManager deliberately configures NO LocalOriginRoot, which disables
	// filesystem origins entirely — the stricter half of the same gate.
	s := &Server{Manager: newRealManager(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"sneaky","mode":"subscribe","origin":{"url":"file:///etc/definitely-not-allowed.git"}}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Origin not allowed"`) {
		t.Fatalf("want the same title PUT /origin uses: %s", rec.Body.String())
	}
	// The DETAIL must not repeat the title. A wrapped sentinel reads well in a
	// log and badly in a problem document, where the reader would get "Origin
	// not allowed" and then "origin not allowed: …" before reaching the part
	// that says which path and which root.
	detail := problemDetail(t, rec)
	if strings.HasPrefix(strings.ToLower(detail), "origin not allowed") {
		t.Fatalf("detail repeats the title: %q", detail)
	}
	// …and it still says the thing the reader can act on.
	if !strings.Contains(detail, "local_origin_root") {
		t.Fatalf("detail lost the actionable part: %q", detail)
	}
	// And no job was started: a refused preflight must not leave a create
	// running behind the 4xx.
	list := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/repo-creates", nil))
	if got := embeddedCreates(t, list.Body.Bytes()); len(got) != 0 {
		t.Fatalf("a refused preflight started a create anyway: %v", got)
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
