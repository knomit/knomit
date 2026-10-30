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

// A spilled factset the sweeper evicted (the size cap) reads exactly like a
// cursor that never existed: the 404, never a 500. So does a cursor whose
// factset was lost while its row remains (in-memory factsets do not survive their
// store closing).
//
// Sabotage: make openFactSet answer a missing factset with an error instead of
// ErrCursorExpired, and the second cursor is a 500.
func TestFactSet_EvictedCursorIs404(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	seedShared(t, alpha)
	h := asPrincipal("u1", newTestRouter(m))
	other := asPrincipal("u2", newTestRouter(m))
	_, _, ghost := getFactSet(t, h, factsetBase+"?cursor=00000000-0000-4000-8000-000000000000")

	restore := sessions.SetFactSetSpillBytesForTest(1) // every factset spills
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&limit=1")
	restore()
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		st, err := svc.ToolSession().SweepFactSetFiles(context.Background(), sessions.FactSetSweepPolicy{
			Now: time.Now(), IdleTTL: time.Hour, Grace: time.Minute, MaxBytes: 1,
		})
		require.NoError(t, err)
		require.Equal(t, 1, st.Evicted)
	}))
	code, _, evicted := getFactSet(t, h, factsetBase+"?cursor="+*p1.Cursor)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghost, evicted)

	code, p2, body := getFactSet(t, other, factsetBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, alpha.WithRead(func(svc *store.Service) {
		svc.ToolSession().CloseFactSets() // the in-memory factset is lost; its row stays
		info, err := svc.ToolSession().GetFactSet(context.Background(), *p2.Cursor)
		require.NoError(t, err)
		require.NotNil(t, info)
	}))
	_, _, ghost2 := getFactSet(t, other, factsetBase+"?cursor=00000000-0000-4000-8000-000000000000")
	code, _, missing := getFactSet(t, other, factsetBase+"?cursor="+*p2.Cursor)
	require.Equal(t, http.StatusNotFound, code, missing)
	require.Equal(t, ghost2, missing)
}
