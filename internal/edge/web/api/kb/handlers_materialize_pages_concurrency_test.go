package kb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
)

// walkPages pages one materialization from first to the end without calling
// t.FailNow (it runs on its own goroutine); it returns the facts' paths in the
// order served, or the first non-200 answer.
func walkPages(h http.Handler, base, first string) ([]string, error) {
	var got []string
	u := base + first
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		if rec.Code != http.StatusOK {
			return got, fmt.Errorf("GET %s: %d %s", u, rec.Code, rec.Body.String())
		}
		var p materialize.Page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			return got, err
		}
		for _, n := range p.Facts {
			got = append(got, n.Path)
		}
		if !p.HasMore {
			return got, nil
		}
		u = base + "?cursor=" + url.QueryEscape(*p.Cursor)
	}
	return got, fmt.Errorf("paging does not end")
}

// Concurrent walks, each with its own cursor, must all complete: a cursor
// call must never answer 500 because another walk was writing the session DB
// at the same moment (the dequeue's read-then-write transaction failed with
// "database is locked", which the busy timeout cannot wait out).
//
// Sabotage: make DequeuePaths a DEFERRED transaction again (BeginTx instead
// of BEGIN IMMEDIATE on its own connection) and cursor calls answer 500
// "dequeue error: DequeuePaths delete: database is locked".
func TestMaterializePages_ConcurrentWalksAllComplete(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	want := seedChain(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	const base = "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

	for _, walkers := range []int{2, 5, 10, 20} {
		t.Run(fmt.Sprintf("walkers=%d", walkers), func(t *testing.T) {
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, walkers)
			got := make([][]string, walkers)
			for i := 0; i < walkers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					got[i], errs[i] = walkPages(h, base, "?limit=1")
				}(i)
			}
			close(start)
			wg.Wait()
			for i := 0; i < walkers; i++ {
				require.NoError(t, errs[i], "walk %d", i)
				require.Equal(t, want, got[i], "walk %d: every fact once, in order", i)
			}
		})
	}
}
