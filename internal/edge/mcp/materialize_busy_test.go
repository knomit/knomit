package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

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
