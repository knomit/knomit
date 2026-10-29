package kb

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
)

// A cursor call whose dequeue finds the session store locked past the busy
// timeout answers 503 with Retry-After and a fixed detail (no internal error
// text), consumes nothing, and the same call succeeds once the lock is gone.
// Takes the busy timeout (5 s) to run.
//
// Sabotage: drop the ErrCursorBusy case in serveMaterializePages and the
// answer is the 500 carrying "database is locked".
func TestMaterializePages_BusySessionStoreIs503WithRetryAfter(t *testing.T) {
	mainPath := filepath.Join(t.TempDir(), "alpha.db")
	svc, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	}))
	want := seedChain(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	const base = "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

	code, first, body := getPage(t, h, base+"?limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.NotNil(t, first.Cursor)
	next := base + "?cursor=" + url.QueryEscape(*first.Cursor)

	// Hold the session DB's write lock from another handle.
	ctx := context.Background()
	other, err := sql.Open("sqlite3", model.SessionDBPathFor(mainPath)+"?_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, next, nil))
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	_ = conn.Close()

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, materializeBusyRetryAfter, rec.Header().Get("Retry-After"))
	require.Contains(t, rec.Body.String(), errMaterializeCursorBusy)
	require.NotContains(t, rec.Body.String(), "locked", "no internal error text in the problem")
	require.NotContains(t, rec.Body.String(), "DequeuePaths", "no internal error text in the problem")

	// Nothing was consumed: the same call now serves fact 2, and the walk
	// still ends with every fact once.
	code, p, body := getPage(t, h, next)
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, p.Facts, 1)
	require.Equal(t, want[1], p.Facts[0].Path, "the retried call serves the page the busy one did not")
}
