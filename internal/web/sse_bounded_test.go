package web

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/repos"
)

// Every SSE stream in this package must bound its writes: a deadline before
// each one AND a checked error, so a client that stops draining its socket
// cannot pin the handler. The servers run with WriteTimeout 0 on purpose —
// any non-zero value would cut every stream at the timeout — so nothing else
// bounds a write, and the hubs never block on a slow subscriber.
//
// These are the contract tests for the streams that were unbounded until the
// shared sse.go helper landed. The sessions and logs streams have their own
// equivalents beside their handlers.

// brokenStream runs one SSE request against a recorder that fails every write
// after the first flush, and reports whether the handler returned and whether
// it set a deadline.
func brokenStream(t *testing.T, s *Server, path string) (*streamRecorder, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	// Let the handler get through its opening writes.
	rec.waitFor(t, "the first frame", func(b string) bool { return strings.Contains(b, "event: ") || strings.Contains(b, "data: ") })
	rec.failWrites(errors.New("connection reset by peer"))
	return rec, waitReturn(done)
}

func waitReturn(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func branchEventsServer(t *testing.T) (*Server, *repos.TaskHub) {
	t.Helper()
	hub := repos.NewTaskHub(context.Background())
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "alpha", Hub: hub})
	m := newTestManagerWithRepos(t)
	m.Set("alpha", ri)
	return &Server{Manager: m}, hub
}

// The branch stream is the one every browser tab holds open, so it is the most
// exposed of the lot.
func TestBranchEvents_BoundsItsWrites(t *testing.T) {
	s, hub := branchEventsServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.NewAPIRouter().ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/events", nil).WithContext(ctx))
	}()
	rec.waitFor(t, "the initial status frame", func(b string) bool { return strings.Contains(b, "event: status") })

	rec.mu.Lock()
	deadlines := rec.deadlines
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("handler set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	if _, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
		emit(repos.TaskEvent{Status: "done"})
	}); err != nil {
		t.Fatal(err)
	}

	if !waitReturn(done) {
		t.Fatal("handler kept running after a failed write — it will spin on a dead connection at full event rate")
	}
}

// The jobs stream replays matching snapshot events and then follows the hub,
// returning on the job's terminal event. The bound must not disturb that.
func TestJobEvents_BoundsItsWrites(t *testing.T) {
	s, hub := branchEventsServer(t)
	// The watched job emits once, waits, then emits again — the second emit is
	// what must find the dead socket. Emitting on a DIFFERENT job would prove
	// nothing: this handler filters by job id, so it would never attempt a
	// write at all.
	fire := make(chan struct{})
	id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
		emit(repos.TaskEvent{Status: "running"})
		select {
		case <-fire:
			emit(repos.TaskEvent{Status: "running", Message: "after the break"})
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.NewAPIRouter().ServeHTTP(rec,
			httptest.NewRequest(http.MethodGet, "/repos/alpha/branches/agent:test/synthesis-runs/"+id+"/events", nil).WithContext(ctx))
	}()
	rec.waitFor(t, "the snapshot replay", func(b string) bool { return strings.Contains(b, "event: task") })

	rec.mu.Lock()
	deadlines := rec.deadlines
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("handler set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	close(fire)

	// The job's own terminal event is what this handler returns on normally;
	// a dead socket must end it too, and sooner.
	if !waitReturn(done) {
		t.Fatal("handler kept running after a failed write")
	}
}

// The branch stream must survive the REAL middleware chain: Compress and the
// metrics middleware both wrap the ResponseWriter, and NewResponseController
// reaches the connection only through Unwrap — so a wrapper that stops
// unwrapping would turn "refuse to start" into a silently dead endpoint.
func TestBranchEvents_WorksThroughTheRealMiddlewareChain(t *testing.T) {
	s, hub := branchEventsServer(t)
	srv := httptest.NewServer(s.NewAPIRouter())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/repos/alpha/branches/agent:test/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip") // make Compress wrap the writer
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	want := func(what string, match func(string) bool) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %s", what)
				}
				if match(line) {
					return
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	want("the initial status frame", func(l string) bool { return l == "event: status" })
	if _, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
		emit(repos.TaskEvent{Status: "running", Message: "through the chain"})
	}); err != nil {
		t.Fatal(err)
	}
	want("a live task frame", func(l string) bool { return strings.Contains(l, "through the chain") })
}

// beginSSE backs four origin-session flows (test, preview, apply, commit) and
// is the one sender in this package that does not turn a failed write into a
// return — those procedures are not abortable half-way. What must hold instead
// is that a dead client costs nothing: the first failure latches, and every
// later send is a no-op rather than another blocked write.
func TestBeginSSE_LatchesDeadAndStopsWriting(t *testing.T) {
	rec := newStreamRecorder()
	send, ok := beginSSE(rec)
	if !ok {
		t.Fatal("beginSSE refused a recorder that has Flush and SetWriteDeadline")
	}

	send(map[string]string{"step": "first"})
	if !strings.Contains(rec.body(), "first") {
		t.Fatalf("first event not written: %q", rec.body())
	}
	rec.mu.Lock()
	deadlines, before := rec.deadlines, rec.ResponseRecorder.Body.Len()
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("beginSSE set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	send(map[string]string{"step": "fails"})

	// Everything after the failure must be a no-op: no bytes, and no further
	// deadline calls, which is what proves it is short-circuiting rather than
	// attempting each write and failing.
	rec.failWrites(nil) // let writes succeed again; a latched stream still must not write
	send(map[string]string{"step": "after"})

	rec.mu.Lock()
	after, deadlinesAfter := rec.ResponseRecorder.Body.Len(), rec.deadlines
	rec.mu.Unlock()
	if after != before {
		t.Errorf("stream wrote %d more bytes after a failed write; it should be latched dead", after-before)
	}
	if deadlinesAfter != deadlines+1 {
		t.Errorf("deadline calls went %d → %d; after latching there should be none", deadlines, deadlinesAfter)
	}
}
