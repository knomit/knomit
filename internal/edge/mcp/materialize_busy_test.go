package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
)

// A knomit_materialize resume whose dequeue finds the session store locked
// past the busy timeout is a tool error telling the caller to retry the SAME
// call (not the expiry text, not the raw "database is locked"), and the retry
// serves the page. Takes the busy timeout (5 s) to run.
//
// Sabotage: drop the ErrCursorBusy case in materializeResume and the text is
// the raw "dequeue error: ... database is locked".
func TestMaterialize_BusySessionStoreIsRetryable(t *testing.T) {
	mainPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)

	first := matCall(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "audience": "human", "limit": 2})
	require.True(t, first.HasMore)
	require.NotNil(t, first.Cursor)

	other, err := sql.Open("sqlite3", model.SessionDBPathFor(mainPath)+"?_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	require.NoError(t, err)

	res, text := matCallRaw(t, ctx, nil, map[string]any{"cursor": *first.Cursor})
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()
	require.True(t, res.IsError)
	require.Equal(t, errSessionBusy, text)

	res, text = matCallRaw(t, ctx, nil, map[string]any{"cursor": *first.Cursor})
	require.False(t, res.IsError, text)
	var p matPage
	require.NoError(t, json.Unmarshal([]byte(text), &p), text)
	require.Equal(t, first.NTo+1, p.NFrom, "the retry serves the page the busy call did not")
}

// N7 (review of PR #692): a factset failure that is not the caller's to fix is
// a FIXED tool text, as on REST: the internal error (SQL, a file path) is
// only logged. The caller's own input errors and the factset's bounds are
// still named.
//
// Sabotage: return err.Error() for the default case, and the path leaks.
func TestFactSetToolError_InternalTextNeverLeaks(t *testing.T) {
	internal := fmt.Errorf("spill factset: open: /home/someone/.knomit/x.db.factsets/abc.db: %w", errors.New("disk I/O error"))
	res := factsetToolError(internal)
	require.True(t, res.IsError)
	text := resultText(t, res)
	require.Equal(t, errFactSetFailed, text)
	require.NotContains(t, text, "/home/")

	invalid := factsetToolError(fmt.Errorf("%w: facts must be between 1 and 500, got 0", materialize.ErrInvalid))
	require.Contains(t, resultText(t, invalid), "facts must be between 1 and 500")
	require.Equal(t, errMaterializeExpired, resultText(t, factsetToolError(materialize.ErrCursorExpired)))
	require.Equal(t, errSessionBusy, resultText(t, factsetToolError(materialize.ErrStoreBusy)))
}

// N3 (review of #696): a single-fact failure that is not the caller's to fix
// is a FIXED tool text, as factset failures are: the store's error (here
// "no such table: ref_edges") is only logged. The caller's own mistakes keep
// their text: a bad limit on a resume, a file that is not a fact.
//
// Sabotage: return err.Error() from materializeToolError's default case, and
// the SQL text reaches the caller.
func TestMaterialize_InternalTextNeverLeaks(t *testing.T) {
	mainPath := filepath.Join(t.TempDir(), "k.db")
	svc, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "test", UID: nextTestRepoUID(), AgentBranch: "agent/test", Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)

	first := matCall(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "audience": "human", "limit": 2})
	require.NotNil(t, first.Cursor)
	res, text := matCallRaw(t, ctx, nil, map[string]any{"cursor": *first.Cursor, "limit": 0})
	require.True(t, res.IsError)
	require.Equal(t, "limit must be at least 1", text, "the caller's own argument error keeps its text")

	res, text = matCallRaw(t, ctx, nil, map[string]any{"file": "kb/t1/none.md", "audience": "human"})
	require.True(t, res.IsError)
	require.NotEqual(t, errMaterializeFailed, text, "a missing fact is the caller's to fix: named")

	db, err := sql.Open("sqlite3", mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`DROP TABLE ref_edges`)
	require.NoError(t, err)
	res, text = matCallRaw(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "audience": "human"})
	require.True(t, res.IsError)
	require.Equal(t, errMaterializeFailed, text)
	require.NotContains(t, text, "ref_edges")

	require.Equal(t, materialize.ErrNotReady.Error(), resultText(t, materializeToolError(fmt.Errorf("plan: %w", materialize.ErrNotReady), "")))
	require.Equal(t, errSessionBusy, resultText(t, materializeToolError(fmt.Errorf("x: %w", materialize.ErrStoreBusy), "")))
	require.Equal(t, errMaterializeExpired, resultText(t, materializeToolError(materialize.ErrCursorExpired, "")))
	require.Contains(t, resultText(t, materializeToolError(fmt.Errorf("%w: depth must be >= 0", materialize.ErrInvalid), "")), "depth must be >= 0")
}
