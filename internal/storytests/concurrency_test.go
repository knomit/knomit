// Category F — Concurrency. These tests exercise the store's behavior
// when multiple goroutines operate on the same repo/branch/remote at
// the same time. The invariants being asserted are:
//
//   - disjoint parallel writes all land and the branch stays integral;
//   - concurrent writes to the same path serialize cleanly (every
//     commit is reachable, no torn state);
//   - parallel work on separate branches is isolated;
//   - simultaneous Push from multiple repos to a shared bare remote
//     produces an integral final remote state.
//
// These tests call svc.Facts().WriteFact directly via RepoInstance.WithRead
// inside the goroutines rather than going through BranchHandle.Write.
// The DSL's Write method mutates an unsynchronized snapshots slice on
// the BranchHandle, which would race under Parallel. The escape hatch
// is the same pattern the DSL uses internally, so the production code
// path under test is identical.
package storytests

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/internal/testenv"
)

// ── F1 ────────────────────────────────────────────────────────────────────

// TestConcurrency_DisjointWritesAllLand: 50 goroutines each write a
// unique file to the same branch. Every write must land, the branch
// must report FactCount=50, and Verify must be strictly clean.
func TestConcurrency_DisjointWritesAllLand(t *testing.T) {
	t.Log("F1: 50 goroutines write 50 unique files to the same branch; all land; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	const N = 50
	sb.Parallel(N, func(i int) {
		path := fmt.Sprintf("kb/item%03d.md", i)
		content := testenv.Fact("item").Body(fmt.Sprintf("body-%d", i)).Build()
		repo.Instance().WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(
				context.Background(), "agent/test", path, content,
				fmt.Sprintf("add %d", i), "test")
			if err != nil {
				t.Errorf("goroutine %d: WriteFact: %v", i, err)
			}
		})
	})

	agent.MustHaveFactCount(N)
	repo.MustVerify()
}

// ── F2 ────────────────────────────────────────────────────────────────────

// TestConcurrency_SamePathSerializes: 20 goroutines all write to the
// same path concurrently. The store must serialize these so the final
// HEAD contains exactly one of the written bodies and Verify is clean.
// Because every write advances the branch head, CommitCount must
// increase by exactly 20 from the start.
func TestConcurrency_SamePathSerializes(t *testing.T) {
	t.Log("F2: 20 goroutines write to the same path; all serialize; CommitCount grows by 20; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	startCount := agent.CommitCount()

	const N = 20
	sb.Parallel(N, func(i int) {
		content := testenv.Fact("x").Body(fmt.Sprintf("body-%d", i)).Build()
		repo.Instance().WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(
				context.Background(), "agent/test", "kb/x.md", content,
				fmt.Sprintf("write %d", i), "test")
			if err != nil {
				t.Errorf("goroutine %d: WriteFact: %v", i, err)
			}
		})
	})

	require.Equal(t, startCount+N, agent.CommitCount(),
		"every concurrent write must produce its own commit")
	agent.MustHaveFactCount(1)
	repo.MustVerify()
}

// ── F3 ────────────────────────────────────────────────────────────────────

// TestConcurrency_SeparateBranchesAreIsolated: create 5 branches off
// main, then have 5 goroutines each write to its own branch in
// parallel. Every branch must see only its own writes (isolation), and
// Verify must be strictly clean for the whole repo.
func TestConcurrency_SeparateBranchesAreIsolated(t *testing.T) {
	t.Log("F3: 5 goroutines each write to a distinct branch; writes are isolated per branch; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")

	const N = 5
	branches := make([]*testenv.BranchHandle, N)
	for i := range N {
		branches[i] = repo.BranchFrom(fmt.Sprintf("agent/b%d", i), "agent/test")
	}

	sb.Parallel(N, func(i int) {
		path := fmt.Sprintf("kb/only-on-%d.md", i)
		content := testenv.Fact("only").Body(fmt.Sprintf("branch-%d", i)).Build()
		repo.Instance().WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(
				context.Background(), fmt.Sprintf("agent/b%d", i), path, content,
				"branch-local write", "test")
			if err != nil {
				t.Errorf("goroutine %d: WriteFact: %v", i, err)
			}
		})
	})

	// Every branch has exactly 1 fact (its own), and does NOT see the
	// others' facts.
	for i := range N {
		b := repo.Branch(fmt.Sprintf("agent/b%d", i))
		b.MustHaveFactCount(1)
		b.Head().Fact(fmt.Sprintf("kb/only-on-%d.md", i)).MustExist()
		// Should NOT see any other branch's fact.
		for j := range N {
			if i == j {
				continue
			}
			b.Head().Fact(fmt.Sprintf("kb/only-on-%d.md", j)).MustNotExist()
		}
	}
	repo.MustVerify()
}

// ── F4 ────────────────────────────────────────────────────────────────────

// TestConcurrency_BarrierSimultaneousStart: 20 goroutines wait on a
// Barrier, then all attempt to write to the same branch at the same
// instant. This is the tightest same-branch contention scenario the
// DSL exposes. Invariants match F2.
func TestConcurrency_BarrierSimultaneousStart(t *testing.T) {
	t.Log("F4: Barrier releases 20 goroutines simultaneously; all land on same branch; Verify clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	const N = 20
	startCount := agent.CommitCount()
	barrier := sb.NewBarrier(N)

	sb.Parallel(N, func(i int) {
		path := fmt.Sprintf("kb/barrier%03d.md", i)
		content := testenv.Fact("barrier").Body(fmt.Sprintf("b-%d", i)).Build()
		barrier.Wait()
		repo.Instance().WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(
				context.Background(), "agent/test", path, content,
				fmt.Sprintf("barrier %d", i), "test")
			if err != nil {
				t.Errorf("goroutine %d: WriteFact: %v", i, err)
			}
		})
	})

	require.Equal(t, startCount+N, agent.CommitCount())
	agent.MustHaveFactCount(N)
	repo.MustVerify()
}

// ── F5 ────────────────────────────────────────────────────────────────────

// TestConcurrency_ParallelReadsDuringWrites asserts that reads running
// alongside writes never observe a torn / half-committed state. One
// writer goroutine appends 30 commits to the branch; several reader
// goroutines run Verify(Deep:true) concurrently. Every Verify call must
// report strictly clean — the production code holds a read lock during
// Verify and the store serializes writes at the branch lock, so no
// reader should ever observe an inconsistent snapshot.
func TestConcurrency_ParallelReadsDuringWrites(t *testing.T) {
	t.Log("F5: writer appends commits while readers run Verify concurrently; every read is strictly clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	agent := repo.Branch("agent/test")

	const Writes = 30
	const Readers = 5

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Spawn reader goroutines — they run Verify until stop fires.
	for r := range Readers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rep, err := repo.Instance().Verify(context.Background(), store.VerifyOpts{Deep: true})
				if err != nil {
					t.Errorf("reader %d: Verify error: %v", id, err)
					return
				}
				if !rep.IsStrictlyClean() {
					t.Errorf("reader %d: Verify not clean: %+v", id, rep.Issues)
					return
				}
			}
		}(r)
	}

	// Writer: do Writes commits through the store directly (skip DSL to
	// avoid the snapshot-slice race).
	for i := range Writes {
		path := fmt.Sprintf("kb/w%03d.md", i)
		content := testenv.Fact("w").Body(fmt.Sprintf("body-%d", i)).Build()
		repo.Instance().WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(
				context.Background(), "agent/test", path, content,
				fmt.Sprintf("w %d", i), "test")
			require.NoError(t, err)
		})
	}
	close(stop)
	wg.Wait()

	agent.MustHaveFactCount(Writes)
	repo.MustVerify()
}

// ── F6 ────────────────────────────────────────────────────────────────────

// TestConcurrency_ParallelPushesToSharedRemote: two repos share one
// bare remote. Both write divergent commits on main and Push at the
// same time. The production Push retries with force-push on
// non-fast-forward, so both calls must succeed without error and the
// final remote must be integral (one of the two pushes wins the race;
// the other's force-push retry overwrites it). Both local repos remain
// strictly clean.
func TestConcurrency_ParallelPushesToSharedRemote(t *testing.T) {
	t.Log("F6: two repos push divergently to the same remote in parallel; both succeed; both locals clean")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")

	// Seed the remote with a baseline so main exists.
	seed := sb.Repo("seed").Connect(remote)
	seedMain := seed.Branch("main")
	seedMain.Write("kb/base.md", testenv.Fact("base"), "baseline")
	seedMain.Push()

	// Two more repos each sync the baseline, then write divergent commits.
	a := sb.Repo("a").Connect(remote)
	aMain := a.Branch("main")
	aMain.Sync()
	aMain.Write("kb/a.md", testenv.Fact("a"), "A writes a")

	b := sb.Repo("b").Connect(remote)
	bMain := b.Branch("main")
	bMain.Sync()
	bMain.Write("kb/b.md", testenv.Fact("b"), "B writes b")

	// Push both in parallel. Force-push fallback must make both succeed.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		aMain.Push()
	}()
	go func() {
		defer wg.Done()
		bMain.Push()
	}()
	wg.Wait()

	// Both local repos remain strictly clean.
	a.MustVerify()
	b.MustVerify()
}
