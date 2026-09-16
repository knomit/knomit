package repos

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// servedKnomitOrigin stands up a knomit store with an ontology and n facts on
// main, served over HTTP by the store's own git handler.
//
// HTTP rather than file://, and knomit's handler rather than a bare git repo,
// because the sideband progress this test is about only exists on knomit's
// endpoint. A file:// remote would exercise none of it.
func servedKnomitOrigin(t *testing.T, n int) string {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "origin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{OntologyPath: string(ont)}, "main"))

	ctx := context.Background()
	for i := range n {
		_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/gotchas/f-%03d.md", i),
			testFactBodyFor(i), fmt.Sprintf("fact %d", i), "")
		require.NoError(t, err)
	}

	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func testFactBodyFor(i int) string {
	return fmt.Sprintf(`---
type: observation
domain: [testing]
confidence: 0.9
---
# fact %d

Body for fact %d, long enough to be worth indexing.
`, i, i)
}

// A subscribe create NARRATES itself end to end: the remote's own sideband
// lines during transfer, the heal's own counts during indexing, and a "done"
// that means INDEXED rather than merely registered.
//
// Run against a manager with background sync ENABLED on purpose.
// DisableBackgroundSync — which most tests in this package set — takes an open
// path that heals SYNCHRONOUSLY and marks the index ready before m.Add
// returns, so every assertion about the index phase would pass vacuously with
// no mirror in the code at all.
func TestStartCreate_SubscribeNarratesTransferAndIndexPhases(t *testing.T) {
	url := servedKnomitOrigin(t, 40)

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
	})
	require.NoError(t, m.Start())
	// Registered AFTER the server's own cleanup so it runs BEFORE it (LIFO):
	// the manager's sync loops must stop talking to the origin before the
	// origin goes away.
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}})

	// Poll the way a client does — a latest-value snapshot, fast enough to see
	// the phases go by.
	var seen []CreateStatus
	var indexAtDone string
	deadline := time.Now().Add(90 * time.Second)
	for {
		st := job.Status()
		if len(seen) == 0 || seen[len(seen)-1] != st {
			seen = append(seen, st)
		}
		if st.State != CreateRunning {
			// Read the repo's ACTUAL index state at the moment the job first
			// reported terminal. This is the independent check on "done means
			// indexed": the job's own IndexState field cannot be the evidence
			// for it, because the mirror writes both.
			if ri := m.Get("sub"); ri != nil {
				indexAtDone, _, _ = ri.IndexStatus()
			}
			break
		}
		require.True(t, time.Now().Before(deadline), "create never finished")
		time.Sleep(25 * time.Millisecond)
	}

	final := seen[len(seen)-1]
	require.Equal(t, CreateDone, final.State, "err=%v", final.Err)
	require.Equal(t, PhaseDone, final.Phase)
	require.Equal(t, 100, final.Pct)
	require.Equal(t, IndexStateReady, final.IndexState)
	require.NotEqual(t, IndexStateIndexing, indexAtDone,
		"the job reported done while the repo was still indexing")

	// TRANSFER: at least one status carrying the remote's own sideband line,
	// indeterminate, with no invented percent behind it.
	var transfer *CreateStatus
	for i, st := range seen {
		if st.Phase == PhaseTransfer && strings.Contains(st.Message, "knomit:") {
			transfer = &seen[i]
			break
		}
	}
	require.NotNil(t, transfer, "no transfer status carried a sideband line; saw %s", summarize(seen))
	require.True(t, transfer.Indeterminate, "a transfer status must not claim a percent")

	// INDEX: at least one status from the mirror, carrying the heal's counts.
	var index *CreateStatus
	for i, st := range seen {
		if st.Phase == PhaseIndex {
			index = &seen[i]
			break
		}
	}
	require.NotNil(t, index, "the create never reported an index phase; saw %s", summarize(seen))
	require.Equal(t, IndexStateIndexing, index.IndexState)
	require.True(t, strings.HasPrefix(index.Message, "indexing "), "got %q", index.Message)
	require.GreaterOrEqual(t, index.Pct, indexPctFloor)
	require.Less(t, index.Pct, 100)

	// And the phases happened in that order.
	require.Less(t, indexOfPhase(seen, PhaseTransfer), indexOfPhase(seen, PhaseIndex))
	require.Less(t, indexOfPhase(seen, PhaseIndex), indexOfPhase(seen, PhaseDone))
}

func indexOfPhase(seen []CreateStatus, phase string) int {
	for i, st := range seen {
		if st.Phase == phase {
			return i
		}
	}
	return -1
}

func summarize(seen []CreateStatus) string {
	var b strings.Builder
	for _, st := range seen {
		fmt.Fprintf(&b, "\n  [%s/%s] pct=%d indet=%v index=%q %q",
			st.Phase, st.Step, st.Pct, st.Indeterminate, st.IndexState, st.Message)
	}
	return b.String()
}
