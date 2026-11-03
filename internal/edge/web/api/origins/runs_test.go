package origins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// The run tests (issue #509): /apply and /commit answer 202 with a link to
// their events, and the link is a GET event stream that replays the run's log
// from the start, resumes after Last-Event-ID, and ends after its last event.

var sseIDs = regexp.MustCompile(`(?m)^id: (\d+)$`)

// eventIDs returns the id: values of an SSE body, in order.
func eventIDs(body string) []string {
	var ids []string
	for _, m := range sseIDs.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// TestApply_PostAnswers202WithTheEventsLink: the POST does not stream. It
// answers 202 with a HAL events link (and the same Location) under the
// session, and the link replays the whole run, numbered, ending with done.
//
// SABOTAGE: revert handleApply to stream on its own response -> red (the
// POST answers 200 text/event-stream, no link).
func TestApply_PostAnswers202WithTheEventsLink(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	post := newStreamRecorder()
	f.s.ServeHTTP(post, newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+f.sess.ID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`)))
	if post.Code != http.StatusAccepted {
		t.Fatalf("POST /apply: status %d, want 202; content type %q; body %s", post.Code, post.Header().Get("Content-Type"), post.body())
	}
	if ct := post.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Fatalf("POST /apply answered an event stream (%q); it must answer the link", ct)
	}
	want := httpx.APIBase + "/repos/alpha/origin-sessions/" + f.sess.ID + "/apply/events"
	if got := runLink(t, post); got != want {
		t.Fatalf("events link %q, want %q", got, want)
	}

	get := followRunLink(t, f.s, post, "")
	if get.Code != http.StatusOK || !strings.Contains(get.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET events: status %d, content type %q", get.Code, get.Header().Get("Content-Type"))
	}
	body := get.body()
	ids := eventIDs(body)
	if len(ids) < 2 || ids[0] != "1" {
		t.Fatalf("want numbered events from id 1; ids %v; body %s", ids, body)
	}
	if !strings.Contains(body, `"phase":"replaying"`) || !strings.HasSuffix(strings.TrimSpace(body), `}`) ||
		!strings.Contains(body[strings.LastIndex(body, "data:"):], `"phase":"done"`) {
		t.Fatalf("want replaying ... done, done last; body %s", body)
	}
}

// TestRunEvents_LateReaderAndReconnect: a reader that subscribes after the
// run has ended gets the whole sequence from the start; a reconnect carrying
// Last-Event-ID resumes after it; one that has seen everything is told 204
// (which stops EventSource reconnecting).
func TestRunEvents_LateReaderAndReconnect(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	post := newStreamRecorder()
	f.s.ServeHTTP(post, newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+f.sess.ID+"/apply",
		strings.NewReader(`{"conflict_strategy":"local_wins"}`)))
	if post.Code != http.StatusAccepted {
		t.Fatalf("POST /apply: status %d; body %s", post.Code, post.body())
	}
	l, ok := f.sm.runs.get("alpha", f.sess.ID, runApply)
	if !ok {
		t.Fatal("no apply run registered")
	}
	select {
	case <-l.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the apply run did not end")
	}

	first := followRunLink(t, f.s, post, "")
	again := followRunLink(t, f.s, post, "")
	if first.body() != again.body() || !strings.Contains(first.body(), "id: 1\n") {
		t.Fatalf("two late readers must both get the whole log from id 1:\n%s\n---\n%s", first.body(), again.body())
	}
	ids := eventIDs(first.body())
	last := ids[len(ids)-1]

	resumed := followRunLink(t, f.s, post, "1")
	if got := eventIDs(resumed.body()); len(got) != len(ids)-1 || got[0] != "2" {
		t.Fatalf("Last-Event-ID 1 must resume at id 2; got ids %v of %v", got, ids)
	}
	if end := followRunLink(t, f.s, post, last); end.Code != http.StatusNoContent {
		t.Fatalf("a reader that has seen every event of an ended run: status %d, want 204; body %s", end.Code, end.body())
	}
}

// TestRunEvents_NoRunIs404: the link of a session that has started no such
// run, or of no session at all, answers 404.
func TestRunEvents_NoRunIs404(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	for _, p := range []string{
		"/repos/alpha/origin-sessions/" + f.sess.ID + "/apply/events",
		"/repos/alpha/origin-sessions/" + f.sess.ID + "/commit/events",
		"/repos/alpha/origin-sessions/no-such-session/apply/events",
	} {
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404; body %s", p, rec.Code, rec.body())
		}
	}
}

// startHeldRun starts a synthetic run of kind on the fixture's session that
// logs "working" and then waits until ctx ends or release is closed, logging
// "cancelled" (an error) or "done". It returns the run's log once the run has
// logged its first event (the POST's 202).
func startHeldRun(t *testing.T, f *commitFixture, kind string, cancelOnDrop bool, release <-chan struct{}) *runLog {
	t.Helper()
	b := hal.URLBuilder{Base: httpx.APIBase}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, l, unuse, ok := f.sm.beginRun(w, r, b, kind)
		if !ok {
			return
		}
		f.sm.startRun(w, r, b, sess, l, unuse, cancelOnDrop, func(ctx context.Context, l *runLog) {
			l.event(map[string]string{"phase": "working"})
			select {
			case <-ctx.Done():
				l.event(map[string]string{"phase": "error", "message": "cancelled"})
			case <-release:
				l.event(map[string]string{"phase": "done"})
			}
		})
	})
	router := chi.NewRouter()
	router.Post("/repos/{repo}/origin-sessions/{sessionID}/"+kind, h)
	r := newStreamRecorder()
	router.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+f.sess.ID+"/"+kind, nil))
	if r.Code != http.StatusAccepted {
		t.Fatalf("held %s: status %d; body %s", kind, r.Code, r.body())
	}
	l, ok := f.sm.runs.get("alpha", f.sess.ID, kind)
	if !ok {
		t.Fatalf("held %s: no run registered", kind)
	}
	return l
}

// TestRun_SecondPostWhileRunningIs409: while a session has a run going, a
// POST /apply or /commit is refused with 409 and the running run's link,
// rather than waiting for the session's use and then running again.
//
// SABOTAGE: drop the active check in runRegistry.reserve -> red (the POST
// waits on the use until the request ends and answers 404).
func TestRun_SecondPostWhileRunningIs409(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	release := make(chan struct{})
	l := startHeldRun(t, f, runApply, true, release)
	defer func() { close(release); <-l.done }()

	for _, step := range []string{"/apply", "/commit"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+f.sess.ID+step,
			strings.NewReader(`{"conflict_strategy":"local_wins"}`)).WithContext(ctx))
		cancel()
		if rec.Code != http.StatusConflict || !strings.Contains(rec.body(), "/apply/events") {
			t.Errorf("POST %s while an apply runs: status %d, want 409 naming the running run's link; body %s", step, rec.Code, rec.body())
		}
	}
}

// TestRun_DroppedSessionEndsTheApplyStream: deleting the session while an
// apply runs cancels the run, and a reader following its events live sees the
// stream end (with the run's error). The session's folder goes only when the
// run has released its use, and the run's log outlives the session.
//
// SABOTAGE: drop the cancelRun call from originSession.retire -> red (the
// stream does not end).
func TestRun_DroppedSessionEndsTheApplyStream(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	// Closed at cleanup (before the manager's, which waits for the run), so a
	// failing run of this test ends instead of hanging the cleanup.
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	l := startHeldRun(t, f, runApply, true, never)
	link := "/repos/alpha/origin-sessions/" + f.sess.ID + "/apply/events"

	got := make(chan *streamRecorder, 1)
	go func() {
		rec := newStreamRecorder()
		f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
		got <- rec
	}()
	f.sm.Delete("alpha", f.sess.ID)

	select {
	case rec := <-got:
		if !strings.Contains(rec.body(), `"phase":"working"`) || !strings.Contains(rec.body(), `"message":"cancelled"`) {
			t.Fatalf("the live reader's stream: %s", rec.body())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deleting the session did not end the apply's event stream")
	}
	<-l.done
	if exists(t, f.sess.TempDir) {
		// finalize runs on release, right after the run; give it a moment.
		deadline := time.Now().Add(5 * time.Second)
		for exists(t, f.sess.TempDir) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if exists(t, f.sess.TempDir) {
			t.Fatal("the dropped session's folder survived its run")
		}
	}
	rec := newStreamRecorder()
	f.s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, link, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.body(), `"message":"cancelled"`) {
		t.Fatalf("the run's events must outlive its session: status %d; body %s", rec.Code, rec.body())
	}
}

// TestRun_DroppedSessionDoesNotStopACommit: a commit run is not cancelled
// when its session is dropped (a store swap must not stop half-way); it runs
// to its end, and the session is finalized after it.
func TestRun_DroppedSessionDoesNotStopACommit(t *testing.T) {
	f := newCommitFixture(t, "", nil)
	release := make(chan struct{})
	var once sync.Once
	releaseRun := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseRun)
	l := startHeldRun(t, f, runCommit, false, release)
	f.sm.Delete("alpha", f.sess.ID)
	select {
	case <-l.done:
		t.Fatal("dropping the session ended the commit run")
	case <-time.After(100 * time.Millisecond):
	}
	if !exists(t, f.sess.TempDir) {
		t.Fatal("the session's folder was removed under its running commit")
	}
	releaseRun()
	<-l.done
	evs, _, _, _ := l.since(0)
	if last := string(evs[len(evs)-1]); !strings.Contains(last, `"phase":"done"`) {
		t.Fatalf("the commit run must end on its own terms; last event %s", last)
	}
}
