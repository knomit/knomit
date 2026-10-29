package kb

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store"
	"knomit/internal/engine/store/sessions"
)

// A spilled book the sweeper evicted (the size cap) reads exactly like a
// cursor that never existed: the 404, never a 500. So does a cursor whose
// book was lost while its row remains (in-memory books do not survive their
// store closing).
//
// Sabotage: make openBook answer a missing book with an error instead of
// ErrCursorExpired, and the second cursor is a 500.
func TestBook_EvictedCursorIs404(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	seedShared(t, alpha)
	h := asPrincipal("u1", newTestRouter(m))
	other := asPrincipal("u2", newTestRouter(m))
	_, _, ghost := getBook(t, h, bookBase+"?cursor=00000000-0000-4000-8000-000000000000")

	restore := sessions.SetBookSpillBytesForTest(1) // every book spills
	code, p1, body := getBook(t, h, bookBase+"?path=kb/u/&limit=1")
	restore()
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		st, err := svc.ToolSession().SweepBookFiles(context.Background(), sessions.BookSweepPolicy{
			Now: time.Now(), IdleTTL: time.Hour, Grace: time.Minute, MaxBytes: 1,
		})
		require.NoError(t, err)
		require.Equal(t, 1, st.Evicted)
	}))
	code, _, evicted := getBook(t, h, bookBase+"?cursor="+*p1.Cursor)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghost, evicted)

	code, p2, body := getBook(t, other, bookBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		svc.ToolSession().CloseBooks() // the in-memory book is lost; its row stays
		info, err := svc.ToolSession().GetBook(context.Background(), *p2.Cursor)
		require.NoError(t, err)
		require.NotNil(t, info)
	}))
	_, _, ghost2 := getBook(t, other, bookBase+"?cursor=00000000-0000-4000-8000-000000000000")
	code, _, missing := getBook(t, other, bookBase+"?cursor="+*p2.Cursor)
	require.Equal(t, http.StatusNotFound, code, missing)
	require.Equal(t, ghost2, missing)
}
