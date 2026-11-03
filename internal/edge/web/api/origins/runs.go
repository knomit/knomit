package origins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
)

// RUNS (issue #509). /apply and /commit are POSTs that START a run and answer
// 202 with a link to its progress events; the page follows the link with a
// GET event stream. They used to stream progress on the POST's own response,
// which the Windows desktop cannot deliver live: Wails' Windows asset handler
// buffers a response until the handler returns, and only GET event streams
// ride the desktop's knomit-sse Wails Stream. A GET that only READS a buffered
// log is safe to reconnect; a GET that started the write would re-run it on
// every EventSource reconnect.
//
// One run per session at a time. A session's steps take its exclusive use
// (USE on originSession), and the run holds that use for its whole life, so a
// session can never have two runs going at once. The registry enforces it up
// front as well: a POST while the session already has an active run (of
// either kind) is refused with 409, rather than waiting on the use and then
// running a second time. So the events link can be session-scoped:
// .../origin-sessions/{id}/apply/events and .../commit/events.
//
// The log. Every event the run emits is kept, in order, for runRetention
// after the run ends, independent of the session (commit deletes its own
// session when it finishes, and a late reader must still get the outcome).
// A GET replays the log from the start, then follows it live, then ends after
// the last event. Each event carries `id: <n>`, its 1-based position, and a
// reconnect that sends Last-Event-ID (EventSource and the desktop's knomit-sse
// leg both do) resumes after it instead of replaying. A GET on a run that has
// ended and has nothing past the reader's Last-Event-ID answers 204, which
// tells EventSource to stop reconnecting.
//
// The POST waits for the run's FIRST event before answering 202, so a refusal
// that arrives before anything has happened (the commit machine's guards:
// Different knowledge base, Origin not attached, ...) is still a plain HTTP
// problem on the POST, exactly as before; the run is then forgotten.

// runRetention is how long a finished run's events stay readable: the
// session's own idle expiry, so a reader that comes back within the time the
// wizard could still use the session finds the outcome.
const runRetention = sessionExpiry

// runEventsKeepalive is how often an idle run event stream writes the shared
// keepalive comment (a replay waiting on the model, an index job between
// progress ticks), so a proxy does not cut it. Same period as the job streams.
const runEventsKeepalive = 30 * time.Second

// The two kinds of run, which are also the route segments of their links.
const (
	runApply  = "apply"
	runCommit = "commit"
)

// runLog is one run's event log. A run appends; any number of readers follow.
type runLog struct {
	kind string
	repo string

	mu       sync.Mutex
	events   [][]byte
	terminal bool          // an event with phase done or error has been appended
	finished bool          // the run has returned
	wake     chan struct{} // closed and replaced on every change
	refusal  func(http.ResponseWriter, *http.Request)

	opened chan struct{} // closed by the first event
	done   chan struct{} // closed when the run has returned

	hook func(data []byte) // test hook, called after each append; nil outside tests
}

func newRunLog(kind, repo string, hook func([]byte)) *runLog {
	return &runLog{
		kind:   kind,
		repo:   repo,
		wake:   make(chan struct{}),
		opened: make(chan struct{}),
		done:   make(chan struct{}),
		hook:   hook,
	}
}

// event appends v. It reports true always: a log has no client to lose, so a
// run's sends never fail (the shape the old lazyStream had, kept so the run
// code reads as before).
func (l *runLog) event(v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(map[string]string{"phase": "error", "message": "encode progress event: " + err.Error()})
	}
	var head struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal(data, &head)

	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return true
	}
	l.events = append(l.events, data)
	if head.Phase == "done" || head.Phase == "error" {
		l.terminal = true
	}
	if len(l.events) == 1 {
		close(l.opened)
	}
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
	if l.hook != nil {
		l.hook(data)
	}
	return true
}

// isOpened reports whether the run has emitted an event (lazyStream's question:
// may a refusal still be answered as a plain HTTP problem?).
func (l *runLog) isOpened() bool {
	select {
	case <-l.opened:
		return true
	default:
		return false
	}
}

// refuse records a refusal that arrived before any event: the POST answers it
// as a plain HTTP problem. It is ignored once an event has been appended.
func (l *runLog) refuse(write func(http.ResponseWriter, *http.Request)) {
	l.mu.Lock()
	if len(l.events) == 0 {
		l.refusal = write
	}
	l.mu.Unlock()
}

// finish marks the run returned. A run that emitted events but no terminal one
// (a panic, a path that forgot its done) gets an error event, so a reader
// never waits on a run that will say nothing more.
func (l *runLog) finish() {
	l.mu.Lock()
	if l.finished {
		l.mu.Unlock()
		return
	}
	if len(l.events) > 0 && !l.terminal {
		data, _ := json.Marshal(map[string]string{"phase": "error", "message": "the " + l.kind + " ended without a result"})
		l.events = append(l.events, data)
		l.terminal = true
	}
	l.finished = true
	close(l.done)
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}

// since returns the events after the first n (n clamped to the log's length,
// returned as start), whether the run has finished, and the channel the next
// change closes.
func (l *runLog) since(n int) (evs [][]byte, start int, finished bool, wake <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.events) {
		n = len(l.events)
	}
	return l.events[n:len(l.events):len(l.events)], n, l.finished, l.wake
}

func (l *runLog) isFinished() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// runKey names a run: the session and the kind. Session ids are UUIDs, so
// the repo is checked on lookup rather than keyed.
type runKey struct {
	session string
	kind    string
}

// runRegistry holds every session's runs: the active one, and finished ones
// for runRetention. Its mutex is never held with sessionManager.mu or a
// session's mu.
type runRegistry struct {
	mu     sync.Mutex
	runs   map[runKey]*runLog
	timers map[*runLog]*time.Timer
	closed bool
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: map[runKey]*runLog{}, timers: map[*runLog]*time.Timer{}}
}

// reserve registers a new run of kind for the session, unless the session
// already has an active run of either kind: then it returns that run's kind
// and ok false. A reserved run must be ended with drop (it never started) or
// with finish followed by retire.
func (rr *runRegistry) reserve(repo, session, kind string, hook func([]byte)) (l *runLog, busyKind string, ok bool) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.closed {
		return nil, "", false
	}
	for _, k := range []string{runApply, runCommit} {
		if cur := rr.runs[runKey{session, k}]; cur != nil && !cur.isFinished() {
			return nil, k, false
		}
	}
	key := runKey{session, kind}
	if old := rr.runs[key]; old != nil {
		if t := rr.timers[old]; t != nil {
			t.Stop()
			delete(rr.timers, old)
		}
	}
	l = newRunLog(kind, repo, hook)
	rr.runs[key] = l
	return l, "", true
}

// drop forgets a run that never streamed (refused before its first event),
// so its link answers 404 and the POST can be retried.
func (rr *runRegistry) drop(session string, l *runLog) {
	l.finish()
	rr.mu.Lock()
	defer rr.mu.Unlock()
	key := runKey{session, l.kind}
	if rr.runs[key] == l {
		delete(rr.runs, key)
	}
}

// retire keeps a finished run readable for runRetention, then forgets it.
func (rr *runRegistry) retire(session string, l *runLog) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if rr.closed {
		return
	}
	key := runKey{session, l.kind}
	if rr.runs[key] != l {
		return
	}
	rr.timers[l] = time.AfterFunc(runRetention, func() {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		if rr.runs[key] == l {
			delete(rr.runs, key)
		}
		delete(rr.timers, l)
	})
}

// get returns the run of kind for the session in repo, active or retained.
func (rr *runRegistry) get(repo, session, kind string) (*runLog, bool) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	l := rr.runs[runKey{session, kind}]
	if l == nil || l.repo != repo {
		return nil, false
	}
	return l, true
}

// active returns the session's run that has not finished, if any.
func (rr *runRegistry) active(repo, session string) (*runLog, bool) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	for _, k := range []string{runApply, runCommit} {
		if l := rr.runs[runKey{session, k}]; l != nil && l.repo == repo && !l.isFinished() {
			return l, true
		}
	}
	return nil, false
}

// close forgets every run and stops the retention timers (the manager's
// lifetime has ended). Readers following a run still in progress keep
// following it: the run itself was cancelled with the lifetime and finishes.
func (rr *runRegistry) close() {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.closed = true
	for _, t := range rr.timers {
		t.Stop()
	}
	rr.timers = map[*runLog]*time.Timer{}
	rr.runs = map[runKey]*runLog{}
}

// startRun runs fn on its own goroutine, detached from the POST's request
// (the request's values stay; its cancellation does not) and cancelled when
// the manager's lifetime ends. When cancelOnDrop is set the run is also
// cancelled when the session is dropped (Delete, replace, expiry): apply's
// replay into a clone nobody will commit is abandoned, while a commit, whose
// store swap must not stop half-way, runs to its end.
//
// release is the session's use, which the run now owns: it is released when fn
// returns, after which the log is finished and retained.
//
// startRun then waits for the run's first event, or for it to end without
// one, and answers the POST: 202 with the events link, or the refusal the run
// recorded, or a 500 when it ended with neither. A POST whose client went
// away meanwhile answers nothing; the run carries on.
func (sm *sessionManager) startRun(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, sess *originSession,
	l *runLog, release func(), cancelOnDrop bool, fn func(ctx context.Context, l *runLog)) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stopLife := context.AfterFunc(sm.life, cancel)
	if cancelOnDrop {
		sess.setCancelRun(cancel)
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error().Interface("panic", p).Str("repo", l.repo).Str("session_id", sess.ID).Str("run", l.kind).
					Msg("origin session: run panicked")
				l.event(map[string]string{"phase": "error", "message": fmt.Sprintf("the %s failed: internal error", l.kind)})
			}
			if cancelOnDrop {
				sess.setCancelRun(nil)
			}
			stopLife()
			cancel()
			release()
			l.finish()
			sm.runs.retire(sess.ID, l)
		}()
		fn(ctx, l)
	}()

	select {
	case <-l.opened:
	case <-l.done:
	case <-r.Context().Done():
		return
	}
	if l.isOpened() {
		href := b.OriginSessionRunEvents(l.repo, sess.ID, l.kind)
		w.Header().Set("Location", href)
		hal.WriteHAL(w, http.StatusAccepted, map[string]any{
			"session_id": sess.ID,
			"run":        l.kind,
			"_links": hal.LinkMap{
				"events":  {Href: href},
				"session": {Href: b.OriginSession(l.repo, sess.ID)},
			},
		})
		return
	}
	l.mu.Lock()
	refusal := l.refusal
	l.mu.Unlock()
	sm.runs.drop(sess.ID, l)
	if refusal != nil {
		refusal(w, r)
		return
	}
	hal.WriteProblem(w, http.StatusInternalServerError, l.kind+" failed",
		"the "+l.kind+" ended without reporting anything", r.URL.Path)
}

// beginRun is the common front of /apply and /commit: it reserves the run
// (409 while the session already has one), then takes the session's use.
// On false it has answered the request and holds nothing.
func (sm *sessionManager) beginRun(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, kind string) (*originSession, *runLog, func(), bool) {
	repo := chi.URLParam(r, "repo")
	sessionID := chi.URLParam(r, "sessionID")

	// Only for a live session: a session that is gone answers 404 below
	// whether or not a finished run of it is still retained.
	if _, ok := sm.Get(repo, sessionID); !ok {
		hal.WriteProblem(w, http.StatusNotFound, "Session not found", "session not found", r.URL.Path)
		return nil, nil, nil, false
	}
	l, busy, ok := sm.runs.reserve(repo, sessionID, kind, sm.runEventHook)
	if !ok {
		if busy == "" {
			hal.WriteProblem(w, http.StatusNotFound, "Session not found", "session not found", r.URL.Path)
			return nil, nil, nil, false
		}
		href := b.OriginSessionRunEvents(repo, sessionID, busy)
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Session busy",
			"this session already has an "+busy+" running; follow its events link", r.URL.Path,
			map[string]any{"_links": hal.LinkMap{"events": {Href: href}}})
		return nil, nil, nil, false
	}
	// The use pins the session's store and folder for the whole run (USE on
	// originSession): a Delete, expiry or shutdown meanwhile finalizes it only
	// when the run releases it. The wait for it is bounded by this request.
	sess, release, ok := sm.Use(r.Context(), repo, sessionID)
	if !ok {
		sm.runs.drop(sessionID, l)
		hal.WriteProblem(w, http.StatusNotFound, "Session not found", "session not found", r.URL.Path)
		return nil, nil, nil, false
	}
	return sess, l, release, true
}

// handleRunEvents serves GET .../origin-sessions/{sessionID}/{apply,commit}/events:
// the run's events from the start (or after Last-Event-ID), then live, ending
// after its last event. 404 when the session has no such run (none started,
// or its retention has passed); 204 when the run has ended and the reader has
// already seen everything.
func handleRunEvents(sm *sessionManager, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := chi.URLParam(r, "repo")
		sessionID := chi.URLParam(r, "sessionID")
		l, ok := sm.runs.get(repo, sessionID, kind)
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Run not found",
				"this session has no "+kind+" run (none was started, or it ended more than "+runRetention.String()+" ago)", r.URL.Path)
			return
		}
		cursor := 0
		if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cursor = n
			}
		}
		// A Last-Event-ID past the end (an id from an earlier run of this
		// kind) resumes at the end: since clamps it.
		evs, cursor, finished, wake := l.since(cursor)
		if finished && len(evs) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		stream, ok := httpx.StartSSE(w)
		if !ok {
			return
		}
		keepalive := time.NewTicker(runEventsKeepalive)
		defer keepalive.Stop()
		for {
			for _, data := range evs {
				cursor++
				if !stream.Write("id: %d\ndata: %s\n\n", cursor, data) {
					return
				}
			}
			if finished {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				if !stream.Keepalive() {
					return
				}
			case <-wake:
			}
			evs, cursor, finished, wake = l.since(cursor)
		}
	}
}
