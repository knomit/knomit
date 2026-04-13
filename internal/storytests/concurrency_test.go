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
// bare remote. Both write disjoint commits on main (A adds kb/a.md, B
// adds kb/b.md) and Push at the same time. The loser's push sees a
// ref-update conflict, merges origin/main into its local main with
// "local wins" semantics (which preserves A's non-overlapping changes
// since they don't touch the same paths), and retries. Both pushes
// must succeed, both locals remain clean, and an observer cloning the
// remote after the dust settles must see all three files — baseline,
// A's file, and B's file.
func TestConcurrency_ParallelPushesToSharedRemote(t *testing.T) {
	t.Log("F6: two repos push disjoint commits in parallel; both succeed via fetch-merge-retry; observer sees all files")
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

	// Push both in parallel. Whichever loses the ref-update race must
	// fetch-merge-retry under StrategyLocalWins and still succeed.
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

	// Bring both locals up to date — whichever lost the ref race
	// already merged; the winner still needs to Sync to see the
	// loser's merge commit. After that, both mains must see all
	// three files.
	aMain.Sync()
	bMain.Sync()
	for _, repo := range []*testenv.RepoHandle{a, b} {
		head := branchHead(t, repo, "main")
		snap := &testenv.Snapshot{Commit: head, Branch: repo.Branch("main")}
		snap.Fact("kb/base.md").MustExist()
		snap.Fact("kb/a.md").MustExist()
		snap.Fact("kb/b.md").MustExist()
	}
}

// branchHead resolves the current git HEAD commit hash for the named
// branch directly via the store API, bypassing the DSL's snapshot
// stack. Needed when a commit was produced inside the store (e.g. via
// Push's internal fetch-merge-retry) without going through a DSL
// mutation, so b.Head() would return a stale pre-push snapshot.
func branchHead(t *testing.T, r *testenv.RepoHandle, branch string) string {
	t.Helper()
	var hash string
	var err error
	r.Instance().WithRead(func(svc *store.Service) {
		hash, err = svc.Branches().HeadCommit(context.Background(), branch)
	})
	if err != nil {
		t.Fatalf("HeadCommit(%s): %v", branch, err)
	}
	return hash
}

// ── F7 ────────────────────────────────────────────────────────────────────

// TestConcurrency_PushContentConflict_LocalWins: two repos both write
// to the same path on main with different content, then push in
// parallel. Both pushes must succeed. The loser of the ref-update race
// reconciles with StrategyLocalWins, which means its version of the
// overlapping path wins — so the final remote content for kb/shared.md
// equals whichever pusher pushed LAST (the loser of the ref-race is
// the winner of the content). Non-overlapping baseline survives.
func TestConcurrency_PushContentConflict_LocalWins(t *testing.T) {
	t.Log("F7: two repos push conflicting content to same path; both succeed; final content matches one of the two")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")

	seed := sb.Repo("seed").Connect(remote)
	seedMain := seed.Branch("main")
	seedMain.Write("kb/base.md", testenv.Fact("base"), "baseline")
	seedMain.Push()

	a := sb.Repo("a").Connect(remote)
	aMain := a.Branch("main")
	aMain.Sync()
	aMain.Write("kb/shared.md", testenv.Fact("shared").Body("A version"), "A writes shared")

	b := sb.Repo("b").Connect(remote)
	bMain := b.Branch("main")
	bMain.Sync()
	bMain.Write("kb/shared.md", testenv.Fact("shared").Body("B version"), "B writes shared")

	barrier := sb.NewBarrier(2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		barrier.Wait()
		aMain.Push()
	}()
	go func() {
		defer wg.Done()
		barrier.Wait()
		bMain.Push()
	}()
	wg.Wait()

	a.MustVerify()
	b.MustVerify()

	// Sync both so they converge on the final remote state, then
	// verify the content resolution. The last pusher's local-wins
	// merge overwrites the first pusher's version, so both
	// mains must now agree on some single content for kb/shared.md.
	aMain.Sync()
	bMain.Sync()

	aHead := &testenv.Snapshot{Commit: branchHead(t, a, "main"), Branch: aMain}
	bHead := &testenv.Snapshot{Commit: branchHead(t, b, "main"), Branch: bMain}

	aShared := aHead.Fact("kb/shared.md").MustExist()
	bShared := bHead.Fact("kb/shared.md").MustExist()
	require.Equal(t, aShared.Raw().Body, bShared.Raw().Body,
		"after both syncs the two repos must converge on the same shared.md body")
	body := aShared.Raw().Body
	if body != "A version" && body != "B version" {
		t.Fatalf("kb/shared.md body = %q; expected \"A version\" or \"B version\"", body)
	}
	// Baseline from the non-overlapping path must survive on both.
	aHead.Fact("kb/base.md").MustExist()
	bHead.Fact("kb/base.md").MustExist()
}

// ── F8 ────────────────────────────────────────────────────────────────────

// TestConcurrency_PushRetryExhaustion: lower the push retry budget to
// 1 attempt, then force two disjoint-commit pushes to race. Exactly
// one wins (the first to update the ref); the other hits the ref-
// update conflict on its single allowed attempt and exhausts the
// retry budget. The failing push must surface the "exhausted N
// attempts" error rather than livelocking or silently succeeding.
func TestConcurrency_PushRetryExhaustion(t *testing.T) {
	t.Log("F8: maxPushAttempts=1 + two racing pushers; loser reports \"exhausted\" error; winner succeeds")
	restore := store.SetMaxPushAttemptsForTest(1)
	defer restore()

	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")

	seed := sb.Repo("seed").Connect(remote)
	seedMain := seed.Branch("main")
	seedMain.Write("kb/base.md", testenv.Fact("base"), "baseline")
	seedMain.Push()

	a := sb.Repo("a").Connect(remote)
	aMain := a.Branch("main")
	aMain.Sync()
	aMain.Write("kb/a.md", testenv.Fact("a"), "A writes")

	b := sb.Repo("b").Connect(remote)
	bMain := b.Branch("main")
	bMain.Sync()
	bMain.Write("kb/b.md", testenv.Fact("b"), "B writes")

	barrier := sb.NewBarrier(2)
	var wg sync.WaitGroup
	var aErr, bErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		barrier.Wait()
		a.Instance().WithRead(func(svc *store.Service) {
			_, aErr = svc.Remote().Push(context.Background(), "main", nil)
		})
	}()
	go func() {
		defer wg.Done()
		barrier.Wait()
		b.Instance().WithRead(func(svc *store.Service) {
			_, bErr = svc.Remote().Push(context.Background(), "main", nil)
		})
	}()
	wg.Wait()

	// Exactly one push succeeds and one fails with the exhaustion error.
	succeeded := 0
	failed := 0
	for _, err := range []error{aErr, bErr} {
		if err == nil {
			succeeded++
			continue
		}
		failed++
		require.Contains(t, err.Error(), "exhausted",
			"failure must be retry-budget exhaustion, got: %v", err)
	}
	require.Equal(t, 1, succeeded, "exactly one push must succeed (aErr=%v, bErr=%v)", aErr, bErr)
	require.Equal(t, 1, failed, "exactly one push must exhaust retries (aErr=%v, bErr=%v)", aErr, bErr)

	a.MustVerify()
	b.MustVerify()
}
