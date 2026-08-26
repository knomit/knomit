package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// This is the wiring test for knomit#123, and it goes through the REAL
// streamable-HTTP transport on purpose.
//
// A test that called actorFromRequest directly would assert that the helper
// composes a string — not that anything CALLS it, and not that a session id
// reaches a handler at all. That is the #117a trap this campaign has now hit
// three times ("a test that calls the helper is not a test of the wiring"). So
// this drives an `initialize` and a `tools/call` over
// mcpserver.NewStreamableHTTPServer — the exact construction
// internal/web/server.go uses — and then reads the pipeline_sessions row the
// call created.
//
// Deleting the `ctx = withActor(ctx, req)` line in ReviewHandler fails this
// test and nothing else in the suite.

// actorE2E stands up the production MCP HTTP stack over a single repo: the
// streamable-HTTP server wrapping the real NewServer, behind a middleware that
// puts the RepoInstance on the request context exactly as web's repo middleware
// does. Returns the test server and the store so the row can be read back.
func actorE2E(t *testing.T) (*httptest.Server, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Seed enough facts to cluster. Without them a start call finds no seeds,
	// completes the session on the spot, and leaves nothing ACTIVE — which
	// makes the displacement half of this file untestable and, worse, would
	// pass vacuously: "no session was displaced" is what an unseeded corpus
	// produces AND what a broken implementation produces.
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		f := fact.NewFact("kb/test/" + slug + ".md")
		f.Title = slug
		f.Body = "body of " + slug
		f.Type = fact.Observation
		f.Domain = []string{"test"}
		f.Confidence = 0.5
		f.Sources = 1
		body, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(context.Background(), "agent/test", f.Path(), body, "seed", "")
		require.NoError(t, err)
	}

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "alpha",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})

	mcpHandler := mcpserver.NewStreamableHTTPServer(NewServer("kb", nil, false))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHandler.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
	}))
	t.Cleanup(ts.Close)
	return ts, svc
}

// rpc posts one JSON-RPC message and returns the body plus the response headers.
func rpc(t *testing.T, ts *httptest.Server, body string, hdrs map[string]string) (string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b), resp.Header
}

// startReview runs initialize + a knomit_review start call, and returns the MCP
// session id the server minted alongside the pipeline session id it opened.
func startReview(t *testing.T, ts *httptest.Server, clientName, clientVersion string) (mcpSessionID, pipelineSessionID string) {
	t.Helper()

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
		`"capabilities":{},"clientInfo":{"name":"` + clientName + `","version":"` + clientVersion + `"}}}`
	_, hdr := rpc(t, ts, initBody, nil)
	mcpSessionID = hdr.Get("Mcp-Session-Id")
	require.NotEmpty(t, mcpSessionID, "the transport must mint a session id at initialize")

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_review","arguments":{}}}`
	raw, _ := rpc(t, ts, callBody, map[string]string{"Mcp-Session-Id": mcpSessionID})

	// The handler must have RUN. A tools/call rejected by the transport (a
	// missing or bad session id is rejected before dispatch) returns a bare
	// error body, and every assertion below would then be comparing absence
	// against absence.
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(raw), &env), "tools/call did not return JSON-RPC: %s", raw)
	require.NotEmpty(t, env.Result.Content, "tools/call produced no content: %s", raw)
	require.False(t, env.Result.IsError, "review returned an error: %s", env.Result.Content[0].Text)

	var res struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(env.Result.Content[0].Text), &res))
	require.NotEmpty(t, res.SessionID, "review must report the session it opened: %s", env.Result.Content[0].Text)
	return mcpSessionID, res.SessionID
}

// TestReviewHandler_RecordsOpeningCallerOnTheSessionRow: a review session opened
// over MCP carries the opening call's correlation handle on its
// pipeline_sessions row — which is the whole of #123. Asserted against the ROW,
// not against the handler's return value: the row is what a forensic query reads
// an hour later, and it is the only thing that survives the turn.
func TestReviewHandler_RecordsOpeningCallerOnTheSessionRow(t *testing.T) {
	ts, svc := actorE2E(t)

	mcpSessionID, pipelineSessionID := startReview(t, ts, "knomit-actor-e2e", "4.5.6")

	sess, err := svc.Pipeline().GetPipelineSession(t.Context(), pipelineSessionID)
	require.NoError(t, err)
	require.NotNil(t, sess, "the session row must exist")

	require.Equal(t,
		"mcp-session:"+mcpSessionID+" client:knomit-actor-e2e/4.5.6",
		sess.CreatedBy,
		"the row must name the caller that opened it")
}

// The handle must identify THIS caller, not merely be non-empty. Two clients
// opening two sessions must leave two DIFFERENT handles: an implementation that
// stamped a constant, or that recorded the process rather than the request,
// passes the test above and fails this one — and would answer the forensic
// question confidently and wrongly, which is worse than the blank #123 reports.
//
// Two separate servers, because within one repo+tool+branch the second start
// abandons the first (that is the work-stealing model #123 exists to make
// legible) and both rows would still be readable but the test would be about
// displacement rather than attribution.
func TestReviewHandler_DistinctCallersGetDistinctHandles(t *testing.T) {
	tsA, svcA := actorE2E(t)
	tsB, svcB := actorE2E(t)

	mcpA, pipeA := startReview(t, tsA, "client-alpha", "1.0.0")
	mcpB, pipeB := startReview(t, tsB, "client-beta", "2.0.0")
	require.NotEqual(t, mcpA, mcpB, "the fixture must present two distinct callers")

	rowA, err := svcA.Pipeline().GetPipelineSession(t.Context(), pipeA)
	require.NoError(t, err)
	rowB, err := svcB.Pipeline().GetPipelineSession(t.Context(), pipeB)
	require.NoError(t, err)

	require.NotEqual(t, rowA.CreatedBy, rowB.CreatedBy, "two callers must not share one handle")
	require.Contains(t, rowA.CreatedBy, "client:client-alpha/1.0.0")
	require.Contains(t, rowB.CreatedBy, "client:client-beta/2.0.0")
	require.Contains(t, rowA.CreatedBy, mcpA)
	require.Contains(t, rowB.CreatedBy, mcpB)
}

// startReviewEnvelope is startReview's sibling: it returns the whole decoded
// start envelope rather than just the session id, for assertions about what the
// winner is TOLD rather than what the row records.
func startReviewEnvelope(t *testing.T, ts *httptest.Server, clientName, clientVersion string) (mcpSessionID string, env map[string]any) {
	t.Helper()
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` +
		`"capabilities":{},"clientInfo":{"name":"` + clientName + `","version":"` + clientVersion + `"}}}`
	_, hdr := rpc(t, ts, initBody, nil)
	mcpSessionID = hdr.Get("Mcp-Session-Id")
	require.NotEmpty(t, mcpSessionID)

	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_review","arguments":{}}}`
	raw, _ := rpc(t, ts, callBody, map[string]string{"Mcp-Session-Id": mcpSessionID})

	var rpcEnv struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	require.NoErrorf(t, json.Unmarshal([]byte(raw), &rpcEnv), "tools/call did not return JSON-RPC: %s", raw)
	require.NotEmpty(t, rpcEnv.Result.Content, "tools/call produced no content: %s", raw)
	require.False(t, rpcEnv.Result.IsError, "review returned an error: %s", rpcEnv.Result.Content[0].Text)

	require.NoError(t, json.Unmarshal([]byte(rpcEnv.Result.Content[0].Text), &env))
	require.NotEmpty(t, env["session_id"], "review must report the session it opened")
	return mcpSessionID, env
}

// Within one repo+tool+branch there is exactly ONE session, and starting a new
// one silently displaces whatever was in flight. #113 made the winner's envelope
// say WHAT it displaced; on its own that is an id with nothing behind it, since
// the loser's row is reapable and a resuming caller cannot look it up later.
// This pins the other half: the envelope says WHOSE session it took.
//
// Both starts go through the SAME server, because sharing the repo+tool+branch
// is precisely what makes the second displace the first.
func TestReviewHandler_EnvelopeNamesWhoWasDisplaced(t *testing.T) {
	ts, _ := actorE2E(t)

	firstMCP, firstEnv := startReviewEnvelope(t, ts, "the-loser", "1.0.0")
	// The fixture must actually reach the displacement branch: only an ACTIVE
	// session gets abandoned, so a first session that completed on the spot
	// would make every assertion below compare absence against absence.
	require.NotContains(t, firstEnv, "done",
		"the first session must still be in flight, or there is nothing to displace")
	require.Contains(t, firstEnv, "item", "the first session must hold a work item")

	// The first start displaced nothing, so it must say nothing — an
	// unconditional field would make "displaced someone" indistinguishable
	// from "went first".
	require.NotContains(t, firstEnv, "abandoned_session")
	require.NotContains(t, firstEnv, "abandoned_session_created_by")

	_, secondEnv := startReviewEnvelope(t, ts, "the-winner", "2.0.0")

	require.Equal(t, firstEnv["session_id"], secondEnv["abandoned_session"],
		"the winner must name the session it displaced")
	require.Equal(t,
		"mcp-session:"+firstMCP+" client:the-loser/1.0.0",
		secondEnv["abandoned_session_created_by"],
		"and must name WHO opened it — the loser's handle, not the winner's")

	// Explicitly the LOSER's handle and not the winner's. An implementation
	// that populated the field from the new session rather than the displaced
	// one produces a well-formed and entirely wrong answer, and the two clients
	// are named differently here precisely so that answer is distinguishable.
	displaced, _ := secondEnv["abandoned_session_created_by"].(string)
	require.Contains(t, displaced, "client:the-loser/1.0.0")
	require.NotContains(t, displaced, "the-winner",
		"the displaced handle must not be the displacing caller's own")
}
