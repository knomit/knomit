package repos

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
func TestCreate_SubscribeNarratesTransferAndIndexPhases(t *testing.T) {
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

	// EVERY event, not a sample of them.
	//
	// This used to poll job.Status() every 25 ms. That reads a LATEST-VALUE
	// snapshot, so an index phase shorter than one poll interval is invisible
	// and `require.NotNil(t, index, …)` fails — which is the reviewer's
	// diagnosis of a single unreproducible failure of this test (F4). The
	// window is real: mirrorIndexing emits once immediately and then sleeps
	// 250 ms, so a heal that finishes in under 25 ms produces exactly one index
	// status that a poller can step over.
	//
	// Collecting from the emit callback removes the window by construction
	// rather than making it less likely — every event is seen, and the test no
	// longer has a timing assumption to violate. Create is called directly for
	// that reason; StartCreate's own job/poll path is covered by
	// TestStartCreate_* in create_job_test.go and by the web list tests.
	var mu sync.Mutex
	var seen []Event
	var indexAtDone string
	ri, err := m.Create(context.Background(),
		CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}},
		func(e Event) {
			mu.Lock()
			seen = append(seen, e)
			mu.Unlock()
			if e.Step == "done" {
				// The repo's ACTUAL index state at the moment the create says
				// done — read from the manager, not from the event. The job's
				// own IndexState cannot be the evidence for "done means
				// indexed", because the mirror writes both.
				if inst := m.Get("sub"); inst != nil {
					indexAtDone, _, _ = inst.IndexStatus()
				}
			}
		})
	require.NoError(t, err)
	require.NotNil(t, ri)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seen)

	final := seen[len(seen)-1]
	require.Equal(t, "done", final.Step)
	require.Equal(t, PhaseDone, final.Phase)
	require.Equal(t, 100, final.Pct)
	require.Equal(t, IndexStateReady, final.IndexState)
	require.NotEqual(t, IndexStateIndexing, indexAtDone,
		"the create reported done while the repo was still indexing")

	// TRANSFER: at least one event carrying the remote's own sideband line,
	// indeterminate, with no percent behind it at all.
	var transfer *Event
	for i, e := range seen {
		if e.Phase == PhaseTransfer && strings.Contains(e.Message, "knomit:") {
			transfer = &seen[i]
			break
		}
	}
	require.NotNil(t, transfer, "no transfer event carried a sideband line; saw %s", summarize(seen))
	require.True(t, transfer.Indeterminate, "a transfer event must not claim a percent")

	// INDEX: at least one event from the mirror, carrying the heal's counts.
	var index *Event
	for i, e := range seen {
		if e.Phase == PhaseIndex {
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

func indexOfPhase(seen []Event, phase string) int {
	for i, e := range seen {
		if e.Phase == phase {
			return i
		}
	}
	return -1
}

func summarize(seen []Event) string {
	var b strings.Builder
	for _, e := range seen {
		fmt.Fprintf(&b, "\n  [%s/%s] pct=%d indet=%v index=%q %q",
			e.Phase, e.Step, e.Pct, e.Indeterminate, e.IndexState, e.Message)
	}
	return b.String()
}

// A filesystem origin outside the local-origin root is refused AT PREFLIGHT,
// and the refusal is the policy sentinel rather than an opaque error.
//
// This behaviour CHANGED with the identity layers. ProbeInitialized's error —
// which is only ever ValidateLocalOrigin's — used to be discarded by an
// `if ierr == nil`, so the gate spoke only once the create was already
// running. It is returned now, which is the better side of the line
// CreatePreflight draws: the gate is a LOCAL POLICY decision about a path this
// process can read directly, so nothing about it is uncertain, nothing can
// change between the probe and the create, and no retry makes a refused path
// allowed. Unreachable and auth-required remotes still fall through, because
// those ARE uncertain.
func TestCreatePreflight_RefusesAnOriginOutsideTheGate(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home: home, OntologyRoot: "kb",
			// A root that exists but does NOT contain the origin below.
			LocalOriginRoot: filepath.Join(home, "allowed"),
		},
		AgentBranch:           "agent/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	outside := filepath.Join(t.TempDir(), "elsewhere.git")
	err := m.CreatePreflight(context.Background(), CreateSpec{
		Name: "sneaky", Mode: "subscribe", Origin: &OriginSpec{URL: "file://" + outside},
	})
	require.ErrorIs(t, err, ErrLocalOriginDenied)
	require.Contains(t, err.Error(), "outside the allowed root")

	// And the create itself refuses too — the preflight is an affordance, not
	// the enforcement point, and the gate must hold on the path that clones.
	_, cerr := m.Create(context.Background(), CreateSpec{
		Name: "sneaky", Mode: "subscribe", Origin: &OriginSpec{URL: "file://" + outside},
	}, nil)
	require.Error(t, cerr)
	require.Nil(t, m.Get("sneaky"))
}
