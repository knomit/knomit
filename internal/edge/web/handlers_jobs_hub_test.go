package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
)

// The job routes read the repo's TaskHub and nothing else. These fixtures run
// REAL hub tasks: a synthesis task that finished (synth-1), a rebuild-op task
// that failed (rebuild-2), and a synthesis task held running (synth-3) until
// the test releases it. Two kinds, three states, and ids that differ, so a
// route answering the wrong job, the wrong kind or an invented one fails.

type jobsFixture struct {
	r       http.Handler
	hub     *repos.TaskHub
	release func()
	done    string // the finished synthesis task
	failed  string // the failed rebuild-op task
	running string // the held synthesis task
}

const failedRebuildMsg = "rebuild exploded: disk full"

func newJobsFixture(t *testing.T) jobsFixture {
	t.Helper()
	hub := repos.NewTaskHub(context.Background())
	t.Cleanup(hub.Shutdown)
	m := newTestManagerWithRepos(t, "empty") // a repo whose hub is nil
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", AgentBranch: "agent/test", Hub: hub,
	}))
	m.Set("beta", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "beta", AgentBranch: "agent/test", Hub: repos.NewTaskHub(context.Background()),
	}))

	f := jobsFixture{r: (&Server{Manager: m}).NewAPIRouter(), hub: hub}
	f.done = runToEnd(t, hub, "synth", repos.TaskEvent{Status: "done", Message: "review complete"})
	f.failed = runToEnd(t, hub, "rebuild", repos.TaskEvent{Status: "error", Message: failedRebuildMsg})

	gate := make(chan struct{})
	started := make(chan struct{})
	id, err := hub.Start("synth", func(ctx context.Context, emit func(repos.TaskEvent)) {
		emit(repos.TaskEvent{Status: "running", Phase: "start", Message: "review starting"})
		close(started)
		select {
		case <-gate:
			emit(repos.TaskEvent{Status: "done", Message: "review complete"})
		case <-ctx.Done():
			emit(repos.TaskEvent{Status: "error", Message: ctx.Err().Error()})
		}
	})
	if err != nil {
		t.Fatalf("start held synth: %v", err)
	}
	<-started
	f.running = id
	var once bool
	f.release = func() {
		if !once {
			once = true
			close(gate)
		}
	}
	t.Cleanup(f.release)
	if f.done == f.running || f.done == f.failed {
		t.Fatalf("fixture ids collide: %s %s %s", f.done, f.failed, f.running)
	}
	return f
}

// runToEnd runs one hub task of op that emits final at once, and waits until
// the hub records it as op's last terminal task.
func runToEnd(t *testing.T, hub *repos.TaskHub, op string, final repos.TaskEvent) string {
	t.Helper()
	id, err := hub.Start(op, func(_ context.Context, emit func(repos.TaskEvent)) { emit(final) })
	if err != nil {
		t.Fatalf("start %s: %v", op, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if ev, ok := hub.Task(id); ok && ev.Status == final.Status {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never reached %s", id, final.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f jobsFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, fromLoopback(httptest.NewRequest(method, path, nil)))
	return rec
}

func requireContentType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != want {
		t.Fatalf("content-type = %q, want %q (body %s)", got, want, rec.Body.String())
	}
}

func requireJobProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, title, detail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	requireContentType(t, rec, "application/problem+json")
	gotTitle, gotDetail := decodeProblem(t, rec)
	if gotTitle != title || gotDetail != detail {
		t.Fatalf("problem = %q / %q, want %q / %q", gotTitle, gotDetail, title, detail)
	}
}

type jobsListBody struct {
	Count int `json:"count"`
	Links map[string]struct {
		Href string `json:"href"`
	} `json:"_links"`
	Embedded struct {
		Jobs []jobEnvelope `json:"jobs"`
	} `json:"_embedded"`
}

func listJobs(t *testing.T, f jobsFixture, path string) jobsListBody {
	t.Helper()
	rec := f.do(t, http.MethodGet, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, "application/hal+json")
	if !strings.Contains(rec.Body.String(), `"jobs":[`) {
		t.Fatalf("jobs must be an array, never null or absent: %s", rec.Body.String())
	}
	var body jobsListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("list body: %v (%s)", err, rec.Body.String())
	}
	if body.Links["self"].Href != path {
		t.Fatalf("self link = %q, want %q", body.Links["self"].Href, path)
	}
	if body.Count != len(body.Embedded.Jobs) {
		t.Fatalf("count %d disagrees with %d jobs", body.Count, len(body.Embedded.Jobs))
	}
	return body
}

const (
	alphaSynth   = "/repos/alpha/branches/agent:test/synthesis-runs"
	alphaRebuild = "/repos/alpha/branches/agent:test/index-rebuilds"
)

// LIST is what the hub holds of the kind's op: the running job first, then
// the last finished one — and nothing of the other kind.
func TestJobs_ListIsTheHubsRunningAndLastFinishedJob(t *testing.T) {
	f := newJobsFixture(t)

	synth := listJobs(t, f, alphaSynth)
	want := []jobEnvelope{
		{ID: f.running, Kind: "synthesis-run", State: "running"},
		{ID: f.done, Kind: "synthesis-run", State: "done"},
	}
	if len(synth.Embedded.Jobs) != len(want) {
		t.Fatalf("synthesis-runs = %+v, want %+v", synth.Embedded.Jobs, want)
	}
	for i := range want {
		if synth.Embedded.Jobs[i] != want[i] {
			t.Fatalf("synthesis-runs[%d] = %+v, want %+v", i, synth.Embedded.Jobs[i], want[i])
		}
	}

	rebuild := listJobs(t, f, alphaRebuild)
	wantRebuild := jobEnvelope{ID: f.failed, Kind: "index-rebuild", State: "error", Error: failedRebuildMsg}
	if len(rebuild.Embedded.Jobs) != 1 || rebuild.Embedded.Jobs[0] != wantRebuild {
		t.Fatalf("index-rebuilds = %+v, want [%+v]", rebuild.Embedded.Jobs, wantRebuild)
	}

	// A hub that ran nothing, and a repo with no hub at all, list nothing.
	for _, path := range []string{
		"/repos/beta/branches/agent:test/synthesis-runs",
		"/repos/empty/branches/agent:test/index-rebuilds",
	} {
		if got := listJobs(t, f, path); got.Count != 0 {
			t.Fatalf("%s = %+v, want an empty list", path, got)
		}
	}
}

// GET answers the hub's job of THAT kind: a known id with its own state, an
// id of the other kind's op or one the hub never held is 404.
func TestJobs_GetReadsTheHubByIDAndKind(t *testing.T) {
	f := newJobsFixture(t)

	for _, c := range []struct {
		path string
		want jobEnvelope
	}{
		{alphaSynth + "/" + f.running, jobEnvelope{ID: f.running, Kind: "synthesis-run", State: "running"}},
		{alphaSynth + "/" + f.done, jobEnvelope{ID: f.done, Kind: "synthesis-run", State: "done"}},
		{alphaRebuild + "/" + f.failed, jobEnvelope{ID: f.failed, Kind: "index-rebuild", State: "error", Error: failedRebuildMsg}},
	} {
		rec := f.do(t, http.MethodGet, c.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", c.path, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, "application/hal+json")
		if got := jobOf(t, rec); got != c.want {
			t.Fatalf("GET %s = %+v, want %+v", c.path, got, c.want)
		}
	}

	requireJobProblem(t, f.do(t, http.MethodGet, alphaRebuild+"/"+f.running),
		http.StatusNotFound, "Job not found", "job "+f.running+" not found")
	requireJobProblem(t, f.do(t, http.MethodGet, alphaSynth+"/synth-99"),
		http.StatusNotFound, "Job not found", "job synth-99 not found")
	requireJobProblem(t, f.do(t, http.MethodGet, "/repos/empty/branches/agent:test/synthesis-runs/"+f.done),
		http.StatusNotFound, "Job not found", "job "+f.done+" not found")
}

// DELETE deletes nothing — the hub can neither cancel a job by id nor forget
// a finished one — so it answers 404 for an unknown job and 409 for a known
// one, and the job is still there afterwards.
func TestJobs_DeleteIs404Or409AndDeletesNothing(t *testing.T) {
	f := newJobsFixture(t)

	requireJobProblem(t, f.do(t, http.MethodDelete, alphaSynth+"/synth-99"),
		http.StatusNotFound, "Job not found", "job synth-99 not found")
	requireJobProblem(t, f.do(t, http.MethodDelete, alphaRebuild+"/"+f.done),
		http.StatusNotFound, "Job not found", "job "+f.done+" not found")
	requireJobProblem(t, f.do(t, http.MethodDelete, alphaSynth+"/"+f.running),
		http.StatusConflict, "Job is running", "job "+f.running+" is still running; cancellation is not supported")
	requireJobProblem(t, f.do(t, http.MethodDelete, alphaRebuild+"/"+f.failed),
		http.StatusConflict, "Job cannot be deleted",
		"job "+f.failed+" has finished; the server keeps the last finished job until the next one finishes")

	if got := listJobs(t, f, alphaSynth); got.Count != 2 {
		t.Fatalf("after DELETE the synthesis list = %+v, want both jobs still held", got)
	}
	if rec := f.do(t, http.MethodGet, alphaRebuild+"/"+f.failed); rec.Code != http.StatusOK {
		t.Fatalf("the failed rebuild must survive DELETE: %d", rec.Code)
	}
}

// serveWithin runs one request on a stream recorder and fails the test if the
// handler has not returned within limit: a stream that never ends is a hang,
// and this is where it shows, fast.
func serveWithin(t *testing.T, r http.Handler, path string, limit time.Duration) *streamRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
	}()
	select {
	case <-done:
		return rec
	case <-time.After(limit):
		cancel()
		<-done
		t.Fatalf("GET %s did not end within %v; body so far:\n%s", path, limit, rec.body())
		return nil
	}
}

func taskFrames(t *testing.T, body string) []repos.TaskEvent {
	t.Helper()
	var out []repos.TaskEvent
	for _, frame := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(frame, "event: task\ndata: ") {
			continue
		}
		var ev repos.TaskEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: task\ndata: ")), &ev); err != nil {
			t.Fatalf("task frame %q: %v", frame, err)
		}
		out = append(out, ev)
	}
	return out
}

// EVENTS for an id the hub does not hold is the documented 404 before any
// stream opens — not a stream that never ends.
func TestJobs_EventsForAnUnknownJobIs404NotAHang(t *testing.T) {
	f := newJobsFixture(t)
	for _, c := range []struct{ path, id string }{
		{alphaSynth + "/synth-99/events", "synth-99"},
		{alphaRebuild + "/" + f.running + "/events", f.running}, // the other kind's job
	} {
		rec := serveWithin(t, f.r, c.path, 2*time.Second)
		requireJobProblem(t, rec.ResponseRecorder, http.StatusNotFound, "Job not found", "job "+c.id+" not found")
	}
}

// EVENTS for a finished job is its terminal frame, and the stream ends.
func TestJobs_EventsForAFinishedJobEndsAtOnce(t *testing.T) {
	f := newJobsFixture(t)
	rec := serveWithin(t, f.r, alphaRebuild+"/"+f.failed+"/events", 2*time.Second)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.body())
	}
	requireContentType(t, rec.ResponseRecorder, "text/event-stream")
	frames := taskFrames(t, rec.body())
	if len(frames) != 1 || frames[0].ID != f.failed || frames[0].Status != "error" || frames[0].Message != failedRebuildMsg {
		t.Fatalf("frames = %+v, want the one error frame of %s", frames, f.failed)
	}
}

// EVENTS for a running job follows it and ends with its terminal frame.
func TestJobs_EventsForARunningJobEndsWithItsTerminal(t *testing.T) {
	f := newJobsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, alphaSynth+"/"+f.running+"/events", nil).WithContext(ctx))
	}()
	rec.waitFor(t, "the running frame", func(b string) bool { return strings.Contains(b, `"status":"running"`) })
	f.release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream did not end after the job finished; body:\n%s", rec.body())
	}
	frames := taskFrames(t, rec.body())
	if len(frames) < 2 || frames[0].Status != "running" || frames[len(frames)-1].Status != "done" {
		t.Fatalf("frames = %+v, want running … done", frames)
	}
	for _, ev := range frames {
		if ev.ID != f.running {
			t.Fatalf("frame of another job on %s's stream: %+v", f.running, ev)
		}
	}
}

// THE PRODUCTION PATH: a rebuild started through POST is recorded by the
// lifecycle machine in the hub, and the list, get and delete routes see it
// while it runs — no registry is involved.
func TestJobs_ALifecycleRebuildIsListedAndReadWhileItRuns(t *testing.T) {
	r, _, release := heldIndexServer(t)
	f := jobsFixture{r: r}
	const base = "/repos/alpha/branches/machine:test/index-rebuilds"

	list := listJobs(t, f, base)
	if len(list.Embedded.Jobs) == 0 || list.Embedded.Jobs[0].State != "running" || list.Embedded.Jobs[0].Kind != "index-rebuild" {
		t.Fatalf("index-rebuilds while one runs = %+v, want the running rebuild first", list.Embedded.Jobs)
	}
	id := list.Embedded.Jobs[0].ID

	rec := f.do(t, http.MethodGet, base+"/"+id)
	if got := jobOf(t, rec); rec.Code != http.StatusOK || got.ID != id || got.State != "running" {
		t.Fatalf("GET %s = %d %+v", id, rec.Code, got)
	}
	requireJobProblem(t, f.do(t, http.MethodDelete, base+"/"+id),
		http.StatusConflict, "Job is running", "job "+id+" is still running; cancellation is not supported")
	if synth := listJobs(t, f, "/repos/alpha/branches/machine:test/synthesis-runs"); synth.Count != 0 {
		t.Fatalf("a rebuild must not list as a synthesis run: %+v", synth)
	}

	release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := jobOf(t, f.do(t, http.MethodGet, base+"/"+id))
		if got.State == "done" {
			break
		}
		if got.State != "running" || time.Now().After(deadline) {
			t.Fatalf("rebuild %s after release = %+v, want done", id, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
