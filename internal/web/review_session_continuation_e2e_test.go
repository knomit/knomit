package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// reviewE2E is the real API router over one writable repo seeded with enough
// facts that a knomit_review start leaves an ACTIVE session holding a work
// item. Every call goes over HTTP JSON-RPC on the unscoped mount, through
// knomit_bind, exactly as a dynamically binding client (a Cowork task) does.
type reviewE2E struct {
	t      *testing.T
	h      http.Handler
	svc    *store.Service
	dbPath string
}

func newReviewE2E(t *testing.T) *reviewE2E {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	dbPath := filepath.Join(t.TempDir(), "alpha.db")
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	for _, slug := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		f := fact.NewFact("kb/architecture/test/" + slug + ".md")
		f.Title = "Embedding model note " + slug
		f.Body = "The embedding model is used for similarity search " + slug
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", f.Path(), body, "seed", "")
		require.NoError(t, err)
	}
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: "uid-alpha", Name: "alpha", State: repos.StateActive, Profile: "code", CreatedAt: 1,
	}))
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", Svc: svc, AgentBranch: "agent/test",
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	}))

	cs := newClientSessionsStore(t)
	m.SetClientSessions(cs)
	s := &Server{Manager: m, ClientSessions: cs, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return &reviewE2E{t: t, h: s.NewAPIRouter(), svc: svc, dbPath: dbPath}
}

// client initializes a fresh MCP session and binds alpha, returning the MCP
// session id and the binding handle — one dynamically bound client.
func (e *reviewE2E) client() (sid, handle string) {
	e.t.Helper()
	_, sid = rpcAt(e.t, e.h, unscopedMount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`)
	require.NotEmpty(e.t, sid)
	return sid, bindHandle(e.t, e.h, sid, "alpha")
}

// reviewTurn is the slice of a knomit_review result these tests read.
type reviewTurn struct {
	SessionID string `json:"session_id"`
	Abandoned string `json:"abandoned_session"`
	Resumed   bool   `json:"resumed"`
	Done      bool   `json:"done"`
	Next      string `json:"next"`
	Item      *struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"item"`
	Progress *struct {
		Completed int `json:"completed"`
		Remaining int `json:"remaining"`
	} `json:"progress"`
}

// review calls knomit_review and returns the decoded turn, or the error text.
func (e *reviewE2E) review(sid string, args map[string]any) (reviewTurn, string) {
	e.t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(e.t, err)
	text, isErr := callToolAt(e.t, e.h, unscopedMount, sid, "knomit_review", string(raw))
	if isErr {
		return reviewTurn{}, text
	}
	var out reviewTurn
	require.NoError(e.t, json.Unmarshal([]byte(text), &out), text)
	return out, ""
}

// start opens (or resumes) a session and requires it to hold a work item.
func (e *reviewE2E) start(sid, handle string) reviewTurn {
	e.t.Helper()
	turn, errText := e.review(sid, map[string]any{"binding": handle})
	require.Empty(e.t, errText)
	require.NotEmpty(e.t, turn.SessionID)
	require.NotNil(e.t, turn.Item, "the fixture must leave an item outstanding")
	return turn
}

// declineAnswer is a well-formed answer to the distill items this corpus plans.
const declineAnswer = `{"synthesize":[],"retract":[],"declined_reason":"no-shared-mechanism"}`

func answerArgs(handle string, turn reviewTurn) map[string]any {
	return map[string]any{
		"binding":    handle,
		"session_id": turn.SessionID,
		"response":   declineAnswer,
		"item_id":    turn.Item.ID,
	}
}

// sessionStatus reads the pipeline_sessions row's status.
func (e *reviewE2E) sessionStatus(id string) string {
	e.t.Helper()
	sess, err := e.svc.Pipeline().GetPipelineSession(context.Background(), id)
	require.NoError(e.t, err)
	require.NotNil(e.t, sess, "session %s must exist", id)
	return sess.Status
}

// An answer that arrives without its session_id is refused, loudly, and
// changes nothing. Before this, the call was taken as a START: it abandoned the
// caller's own session and re-served the same first item with progress reset,
// so an agent that had dropped session_id looped forever with no error.
func TestReviewE2E_AnswerWithoutSessionIDIsRefusedAndChangesNothing(t *testing.T) {
	e := newReviewE2E(t)
	sid, handle := e.client()
	first := e.start(sid, handle)

	for name, args := range map[string]map[string]any{
		"response":         {"binding": handle, "response": declineAnswer},
		"response+item_id": {"binding": handle, "response": declineAnswer, "item_id": first.Item.ID},
		"item_id":          {"binding": handle, "item_id": first.Item.ID},
		"completion_token": {"binding": handle, "completion_token": "tok"},
		"page":             {"binding": handle, "page": 2},
	} {
		t.Run(name, func(t *testing.T) {
			turn, errText := e.review(sid, args)
			require.NotEmpty(t, errText, "must be refused, got a turn: %+v", turn)
			require.Contains(t, errText, "session_id")
			require.Contains(t, errText, "Nothing was applied")
			require.Contains(t, errText, "no session was started or abandoned")
		})
	}

	require.Equal(t, "active", e.sessionStatus(first.SessionID),
		"the refused calls must not have displaced the live session")

	// And the live session still answers normally, with its progress intact.
	next, errText := e.review(sid, answerArgs(handle, first))
	require.Empty(t, errText)
	require.Equal(t, first.SessionID, next.SessionID)
	require.NotNil(t, next.Progress)
	require.Equal(t, first.Progress.Completed+1, next.Progress.Completed,
		fmt.Sprintf("progress must advance from %+v", *first.Progress))
}

// backdate makes a session look idle for d, by rewriting its heartbeat in the
// session DB — the only input the resume window reads.
func (e *reviewE2E) backdate(id string, d time.Duration) {
	e.t.Helper()
	db, err := sql.Open("sqlite3", store.SessionDBPathFor(e.dbPath)+"?_busy_timeout=5000")
	require.NoError(e.t, err)
	defer db.Close()
	at := time.Now().UTC().Add(-d).Format(time.RFC3339)
	res, err := db.Exec(`UPDATE pipeline_sessions SET last_used_at = ? WHERE id = ?`, at, id)
	require.NoError(e.t, err)
	n, _ := res.RowsAffected()
	require.EqualValues(e.t, 1, n)
}

// A second start against a live session with the same scope and effort
// RESUMES it: one session, shared, instead of the second caller silently
// displacing the first. Two parallel tasks on one repo then serialise on the
// work-item claim, and the loser of a race gets a loud stale-item error.
func TestReviewE2E_SecondStartResumesTheLiveSession(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	sid2, h2 := e.client()

	first := e.start(sid1, h1)
	require.False(t, first.Resumed, "the first start opens a session")

	second := e.start(sid2, h2)
	require.True(t, second.Resumed, "the second start must resume, not displace")
	require.Equal(t, first.SessionID, second.SessionID)
	require.Empty(t, second.Abandoned, "resuming displaces nothing")
	require.Equal(t, first.Item.ID, second.Item.ID, "both callers see the current item")
	require.Equal(t, "active", e.sessionStatus(first.SessionID))

	// On a shared session every answer must name its item, so one caller's
	// answer can never be applied to the item the other just advanced to.
	_, errText := e.review(sid2, map[string]any{
		"binding": h2, "session_id": second.SessionID, "response": declineAnswer,
	})
	require.Contains(t, errText, "item_id is required")

	next, errText := e.review(sid1, answerArgs(h1, first))
	require.Empty(t, errText)
	require.Equal(t, 1, next.Progress.Completed)

	// The second caller's answer to the same item is now stale: refused, loudly,
	// and not applied. (This corpus plans one item, so the session has
	// completed; with more items the item_id guard names the current one.)
	_, errText = e.review(sid2, answerArgs(h2, second))
	require.NotEmpty(t, errText, "a stale answer must be refused")
	require.Regexp(t, `is current|not active`, errText)
	completed, _, err := e.svc.Pipeline().PipelineWorkItemStats(context.Background(), first.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, completed, "exactly one answer was claimed")
}

// A client that disconnects mid-review leaves its session active; the same
// user's next start, from a new bridge process, picks it up where it stopped.
func TestReviewE2E_RestartAfterDisconnectResumes(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	first := e.start(sid1, h1)

	req := fromLoopback(httptest.NewRequest(http.MethodDelete, unscopedMount, nil))
	req.Header.Set("Mcp-Session-Id", sid1)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	sid2, h2 := e.client()
	again := e.start(sid2, h2)
	require.True(t, again.Resumed)
	require.Equal(t, first.SessionID, again.SessionID)
	require.Equal(t, first.Item.ID, again.Item.ID)
}

// A start whose scope or effort differs from the live session's cannot resume
// it and must not silently replace it: it is refused with everything the
// caller needs to decide, and the live session is untouched.
func TestReviewE2E_MismatchedStartIsRefused(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	sid2, h2 := e.client()
	first := e.start(sid1, h1)

	for name, args := range map[string]map[string]any{
		"scope":  {"binding": h2, "domain": []string{"test"}},
		"effort": {"binding": h2, "effort": "high"},
	} {
		t.Run(name, func(t *testing.T) {
			_, errText := e.review(sid2, args)
			require.NotEmpty(t, errText)
			require.Contains(t, errText, first.SessionID)
			require.Contains(t, errText, "mcp-session:"+sid1, "names who opened it")
			require.Contains(t, errText, "last used")
			require.Contains(t, errText, "takeover:true")
		})
	}
	require.Equal(t, "active", e.sessionStatus(first.SessionID))
}

// takeover:true abandons the live session and starts a new one, and says
// which session it displaced.
func TestReviewE2E_TakeoverDisplaces(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	sid2, h2 := e.client()
	first := e.start(sid1, h1)

	taken, errText := e.review(sid2, map[string]any{"binding": h2, "takeover": true})
	require.Empty(t, errText)
	require.NotEqual(t, first.SessionID, taken.SessionID)
	require.Equal(t, first.SessionID, taken.Abandoned)
	require.False(t, taken.Resumed)
	require.Equal(t, "abandoned", e.sessionStatus(first.SessionID))

	_, errText = e.review(sid1, answerArgs(h1, first))
	require.Contains(t, errText, "abandoned, not active")
}

// takeover is a start argument; with a session_id it means nothing, so it is
// refused rather than ignored.
func TestReviewE2E_TakeoverWithSessionIDIsRefused(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	first := e.start(sid1, h1)
	args := answerArgs(h1, first)
	args["takeover"] = true
	_, errText := e.review(sid1, args)
	require.Contains(t, errText, "takeover")
	require.Equal(t, "active", e.sessionStatus(first.SessionID))
}

// A session idle longer than the resume window is displaced by the next start,
// as before: its caller is gone, and resuming it would hand a stranger a
// half-read item.
func TestReviewE2E_StaleSessionIsDisplaced(t *testing.T) {
	e := newReviewE2E(t)
	sid1, h1 := e.client()
	sid2, h2 := e.client()
	first := e.start(sid1, h1)

	e.backdate(first.SessionID, repos.DefaultPipelineResumeWindow+time.Minute)

	fresh := e.start(sid2, h2)
	require.False(t, fresh.Resumed)
	require.NotEqual(t, first.SessionID, fresh.SessionID)
	require.Equal(t, first.SessionID, fresh.Abandoned)
	require.Equal(t, "abandoned", e.sessionStatus(first.SessionID))
}

// Every result says what to call next and names the session_id to pass, and on
// the unscoped endpoint says the binding selects the repo and is not the
// session. A finished session says not to call again.
func TestReviewE2E_EveryResultNamesTheSessionToContinue(t *testing.T) {
	e := newReviewE2E(t)
	sid, handle := e.client()
	first := e.start(sid, handle)

	require.Contains(t, first.Next, first.SessionID)
	require.Contains(t, first.Next, "session_id")
	require.Contains(t, first.Next, fmt.Sprintf("item_id=%d", first.Item.ID))
	require.Contains(t, first.Next, "binding")
	require.Contains(t, first.Next, "does not identify this review session")

	done, errText := e.review(sid, answerArgs(handle, first))
	require.Empty(t, errText)
	require.True(t, done.Done)
	require.Contains(t, done.Next, "finished")
}

// Every answer names its item: without item_id the answer is refused and the
// item stays open.
func TestReviewE2E_AnswerWithoutItemIDIsRefused(t *testing.T) {
	e := newReviewE2E(t)
	sid, handle := e.client()
	first := e.start(sid, handle)

	_, errText := e.review(sid, map[string]any{
		"binding": handle, "session_id": first.SessionID, "response": declineAnswer,
	})
	require.Contains(t, errText, "item_id is required")

	next, errText := e.review(sid, answerArgs(handle, first))
	require.Empty(t, errText, "the item is still open to a proper answer")
	require.Equal(t, 1, next.Progress.Completed)
}

// session_id with no response re-serves the current item and changes nothing.
func TestReviewE2E_SessionIDAloneServesTheCurrentItem(t *testing.T) {
	e := newReviewE2E(t)
	sid, handle := e.client()
	first := e.start(sid, handle)

	again, errText := e.review(sid, map[string]any{"binding": handle, "session_id": first.SessionID})
	require.Empty(t, errText)
	require.Equal(t, first.SessionID, again.SessionID)
	require.Equal(t, first.Item.ID, again.Item.ID)
	require.Equal(t, first.Progress.Completed, again.Progress.Completed)
	require.Contains(t, again.Next, first.SessionID)
}
