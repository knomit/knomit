package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

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
	t   *testing.T
	h   http.Handler
	svc *store.Service
}

func newReviewE2E(t *testing.T) *reviewE2E {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
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
	return &reviewE2E{t: t, h: s.NewAPIRouter(), svc: svc}
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
	Done      bool   `json:"done"`
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
