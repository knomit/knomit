package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestQueryResume_RejectsReadSetRebind pins M-2 (backlog B.5): a cursor is a
// frozen view of ONE binding's READ SET. Deleting and recreating a lens under
// the SAME name with a read mount re-pinned to a different branch must NOT let
// an in-flight cursor keep paging — it would silently hydrate read-mount rows
// against the new branch. The rejection must be byte-identical to expiry so a
// caller cannot probe how a shared name's read mounts changed (lenses RFC §7.3).
func TestQueryResume_RejectsReadSetRebind(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)

	// Enough policy facts on both mounts (at agent/test) to force a multi-page
	// fused result set, so the first call returns a cursor.
	seedFedMany(t, ctxA, 15, "Alpha", "a body ", "store")
	seedFedMany(t, ctxB, 15, "Bravo", "b body ", "ui")

	// binding1: repo B mounted at agent/test.
	b1 := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b1, map[string]any{"type": []any{"policy"}, "limit": 5})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.NotNil(t, resp.Cursor, "multi-page federated query must return a cursor")
	cursor := *resp.Cursor

	// binding2: SAME name and SAME write-mount branch, but repo B RE-PINNED to a
	// different branch → only the read-set fingerprint diverges. This is exactly
	// the delete+recreate-under-the-same-name attack M-2 closes.
	b2 := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "main"},
	)
	require.Equal(t, b1.Name(), b2.Name(), "the rebind must keep the same binding name")
	require.Equal(t, b1.WriteMountBranch(), b2.WriteMountBranch(), "and the same write-mount branch")
	require.NotEqual(t, federate.ReadSetFingerprint(b1), federate.ReadSetFingerprint(b2), "only the read set changed")

	rebindResult, rebindText := queryVia(t, b2, map[string]any{"cursor": cursor})
	require.True(t, rebindResult.IsError, "resuming against a re-pinned read set must be rejected")
	require.Contains(t, rebindText, "session expired or not found — omit cursor to start a new query",
		"the M-2 rejection must be byte-identical to the expiry error (RFC §7.3)")

	// Happy path: a freshly REBUILT binding equal by value to b1 still resumes —
	// the fingerprint is compared by value, not pointer identity. The rebind gate
	// ran before any dequeue side effect, so the queue is intact.
	b1again := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	require.Equal(t, federate.ReadSetFingerprint(b1), federate.ReadSetFingerprint(b1again), "an identical rebuild has an identical fingerprint")
	okResult, okText := queryVia(t, b1again, map[string]any{"cursor": cursor})
	require.Falsef(t, okResult.IsError, "resuming an identical rebuilt binding must succeed: %s", okText)
	var okResp queryResponse
	require.NoError(t, json.Unmarshal([]byte(okText), &okResp))
	require.NotEmpty(t, okResp.Facts, "identical rebind returns the remainder")
}

// TestExplainResume_RejectsReadSetRebind is the explain-side M-2 guard. explain
// never fans out — the whole walk lives in the input fact's mount — but the
// cursor still pins the binding's full read set, so re-pinning ANY read mount to
// a different branch under the same name invalidates in-flight explain cursors,
// indistinguishably from expiry (lenses RFC §7.3).
func TestExplainResume_RejectsReadSetRebind(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)

	// A fact on the write repo (A) with a child, so the first explain call
	// enqueues the child and a cursor comes back cheaply.
	childPath := "kb/observations/testing/child.md"
	writeExplainFact(t, ctxA, repoA, childPath, "Child", 0.9, nil)
	rootPath := "kb/observations/testing/root.md"
	writeExplainFact(t, ctxA, repoA, rootPath, "Root", 0.9, []string{childPath})

	b1 := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	ctx1 := repos.WithBinding(context.Background(), b1)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": rootPath}
	result, err := ExplainHandler()(ctx1, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))
	var resp expResp
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.NotNil(t, resp.Cursor, "explain must return a cursor with a child queued")

	// Rebind B to a different branch under the same name; read set diverges.
	b2 := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "main"},
	)
	require.NotEqual(t, federate.ReadSetFingerprint(b1), federate.ReadSetFingerprint(b2))
	ctx2 := repos.WithBinding(context.Background(), b2)
	var mreq mcpgo.CallToolRequest
	mreq.Params.Arguments = map[string]any{"file": rootPath, "cursor": *resp.Cursor}
	mResult, err := ExplainHandler()(ctx2, mreq)
	require.NoError(t, err)
	require.True(t, mResult.IsError, "explain resume against a re-pinned read set must be rejected")
	require.Contains(t, resultText(t, mResult), "session expired or not found — omit cursor to start a new session",
		"the M-2 rejection must be byte-identical to the explain expiry error (RFC §7.3)")

	// Happy path: an identical rebuilt binding resumes (gate ran before dequeue).
	b1again := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	ctx3 := repos.WithBinding(context.Background(), b1again)
	var hreq mcpgo.CallToolRequest
	hreq.Params.Arguments = map[string]any{"file": rootPath, "cursor": *resp.Cursor}
	hResult, err := ExplainHandler()(ctx3, hreq)
	require.NoError(t, err)
	require.False(t, hResult.IsError, resultText(t, hResult))
}

// TestQueryResume_RetriesPastUnreadableWindow pins B2: when a dequeued window is
// entirely unreadable (every row pinned at a commit that no longer resolves) but
// the queue is still non-empty, queryResume must dequeue the next window and
// surface the readable rows in the SAME call — not return facts:[] with
// has_more:true (a spurious empty page). This mirrors explainResume's bounded
// retry loop.
func TestQueryResume_RetriesPastUnreadableWindow(t *testing.T) {
	repoA, ctxA := fedRepo(t)

	// One real, readable fact on the write repo → a valid (path, commit) pin.
	readablePath := "kb/observations/testing/readable.md"
	readableCommit := writeExplainFact(t, ctxA, repoA, readablePath, "Readable", 0.9, nil)

	b := repos.NewBindingForTest(repoA, repos.ReadTarget{RI: repoA, Branch: "agent/test"})

	// Mint a session by hand and enqueue a full window of UNREADABLE rows (bogus
	// commits) ahead of the one readable row, so the first dequeue window
	// (limit 2) yields zero readable rows while the queue is still non-empty.
	state, err := json.Marshal(pagedRowState{Score: 42, CommittedAt: 0})
	require.NoError(t, err)
	var sessID string
	repoA.WithRead(func(svc *store.Service) {
		ts := svc.ToolSession()
		sess, cerr := ts.CreateToolSession(context.Background(), "query", b.WriteMountBranch(), "", b.PinID(), federate.ReadSetFingerprint(b))
		require.NoError(t, cerr)
		sessID = sess.ID
		require.NoError(t, ts.EnqueuePaths(context.Background(), sess.ID, []store.QueueItem{
			{Path: "kb/observations/testing/bogus0.md", CommitHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", SortKey: 0, State: string(state)},
			{Path: "kb/observations/testing/bogus1.md", CommitHash: "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff", SortKey: 1, State: string(state)},
			{Path: readablePath, CommitHash: readableCommit, SortKey: 2, State: string(state)},
		}))
	})

	// Resume with a window size of 2: window 1 is all-unreadable, so the retry
	// must dequeue window 2 and surface the readable row in the same call.
	result, text := queryVia(t, b, map[string]any{"cursor": sessID, "limit": 2})
	require.Falsef(t, result.IsError, "resume failed: %s", text)
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Facts, 1, "the readable row must be returned, not skipped into an empty page")
	require.Equal(t, "Readable", resp.Facts[0].Title)
	require.False(t, resp.HasMore, "queue is drained once the readable row is served")
	require.Nil(t, resp.Cursor, "a drained cursor is niled")
}
