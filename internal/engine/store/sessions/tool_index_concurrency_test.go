package sessions

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/require"
)

func newToolIndexForTest(t *testing.T) *ToolIndex {
	t.Helper()
	db, path, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewToolIndex(db, FactSetsDirFor(path))
}

func queueItems(n int) []model.QueueItem {
	items := make([]model.QueueItem, n)
	for i := range items {
		items[i] = model.QueueItem{Path: fmt.Sprintf("kb/x/%03d.md", i), CommitHash: "c", SortKey: i, State: "{}"}
	}
	return items
}

// Concurrent cursor walks, each on its own session, must never fail with a
// storage error. DequeuePaths reads the queue and then deletes from it; as a
// DEFERRED transaction its first write fails with SQLITE_BUSY ("database is
// locked") when another walk committed after its read began, and the busy
// timeout cannot wait that out. That error surfaced as HTTP 500 on the REST
// materialize pages route (cursor calls only: the first call never reads
// before it writes). Every walk must see all of its items, in order.
func TestDequeuePaths_ConcurrentWalksOnSeparateSessions(t *testing.T) {
	ti := newToolIndexForTest(t)
	ctx := context.Background()

	for _, walkers := range []int{2, 5, 10, 20} {
		t.Run(fmt.Sprintf("walkers=%d", walkers), func(t *testing.T) {
			const perWalk = 30
			ids := make([]string, walkers)
			for i := range ids {
				s, err := ti.CreateToolSession(ctx, "materialize:human:3", "main", "kb/root.md", "repo:pin", "rs", "owner")
				require.NoError(t, err)
				require.NoError(t, ti.EnqueuePaths(ctx, s.ID, queueItems(perWalk)))
				ids[i] = s.ID
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, walkers)
			got := make([][]int, walkers)
			for i := range ids {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					for {
						items, err := ti.DequeuePaths(ctx, ids[i], 3)
						if err != nil {
							errs[i] = err
							return
						}
						if len(items) == 0 {
							return
						}
						for _, it := range items {
							got[i] = append(got[i], it.SortKey)
						}
						// NextPage's tail: QueueSize, then a completion write.
						if _, err := ti.QueueSize(ctx, ids[i]); err != nil {
							errs[i] = err
							return
						}
					}
				}(i)
			}
			close(start)
			wg.Wait()

			for i := range ids {
				require.NoError(t, errs[i], "walk %d: a concurrent cursor walk must not hit a storage error", i)
				require.Len(t, got[i], perWalk, "walk %d saw every item", i)
				for k, v := range got[i] {
					require.Equal(t, k, v, "walk %d: items in sort_key order", i)
				}
			}
		})
	}
}

// Concurrent calls on the SAME cursor must neither fail nor hand one item to
// two callers: the select and the delete are one atomic dequeue.
func TestDequeuePaths_ConcurrentCallsOnOneSession(t *testing.T) {
	ti := newToolIndexForTest(t)
	ctx := context.Background()

	for _, callers := range []int{2, 5, 10, 20} {
		t.Run(fmt.Sprintf("callers=%d", callers), func(t *testing.T) {
			const total = 120
			s, err := ti.CreateToolSession(ctx, "materialize:human:3", "main", "kb/root.md", "repo:pin", "rs", "owner")
			require.NoError(t, err)
			require.NoError(t, ti.EnqueuePaths(ctx, s.ID, queueItems(total)))

			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				seen  = map[int]int{}
				start = make(chan struct{})
				errs  = make([]error, callers)
			)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					for {
						items, err := ti.DequeuePaths(ctx, s.ID, 3)
						if err != nil {
							errs[i] = err
							return
						}
						if len(items) == 0 {
							return
						}
						mu.Lock()
						for _, it := range items {
							seen[it.SortKey]++
						}
						mu.Unlock()
					}
				}(i)
			}
			close(start)
			wg.Wait()

			for i := range errs {
				require.NoError(t, errs[i], "caller %d: a concurrent dequeue must not hit a storage error", i)
			}
			require.Len(t, seen, total, "every item was dequeued")
			for k, n := range seen {
				require.Equal(t, 1, n, "item %d dequeued exactly once", k)
			}
		})
	}
}
