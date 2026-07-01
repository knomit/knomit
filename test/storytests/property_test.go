// Property tests (Phase 4). These are OPT-IN — gated behind the
// KNOMIT_PROPTESTS=1 environment variable so they don't run during
// ordinary `go test ./...`. Each test drives the DSL with a
// deterministic random operation generator seeded from
// KNOMIT_PROPTEST_SEED (or time-based fallback) and asserts an
// invariant that should hold over ANY op sequence — not just
// hand-picked scenarios.
//
// On failure, the seed is logged. A rerun with the same seed replays
// the exact same operations, so every property-test failure is
// reproducible.
package storytests

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// proptestSkip skips the test unless KNOMIT_PROPTESTS=1 is set.
func proptestSkip(t *testing.T) {
	t.Helper()
	if os.Getenv("KNOMIT_PROPTESTS") != "1" {
		t.Skip("property tests disabled; set KNOMIT_PROPTESTS=1 to enable")
	}
}

// proptestSeed returns the seed from KNOMIT_PROPTEST_SEED or a
// time-based default, and logs it for reproducibility.
func proptestSeed(t *testing.T) int64 {
	t.Helper()
	if s := os.Getenv("KNOMIT_PROPTEST_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("invalid KNOMIT_PROPTEST_SEED %q: %v", s, err)
		}
		t.Logf("property test seed (from env): %d", n)
		return n
	}
	n := time.Now().UnixNano()
	t.Logf("property test seed (time-based): %d — set KNOMIT_PROPTEST_SEED=%d to reproduce", n, n)
	return n
}

// ── P1 ────────────────────────────────────────────────────────────────────

// TestProperty_IntegrityHoldsUnderRandomOps drives random write /
// update / delete ops over a small fact set and relies on the
// Storyboard's auto-verify (which runs Deep Verify after every
// mutation) to catch any integrity violation. Any op sequence should
// leave the store strictly clean.
func TestProperty_IntegrityHoldsUnderRandomOps(t *testing.T) {
	proptestSkip(t)
	t.Log("P1: random op sequences, Verify after every step, expect clean")
	seed := proptestSeed(t)

	const iterations = 100
	const stepsPerIter = 50
	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md", "kb/e.md"}

	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("agent/test")

		for step := range stepsPerIter {
			op := gen.Choose([]string{"write", "update", "delete"})
			path := gen.Choose(paths)
			switch op {
			case "write", "update":
				agent.Write(path,
					testenv.Fact(fmt.Sprintf("v%d", step)).Confidence(gen.Float64()),
					fmt.Sprintf("step%d", step))
			case "delete":
				// Guard against deleting a missing path — the production
				// DeleteFact errors on missing, and auto-verify would
				// still leave the store clean, but the DSL t.Fatals.
				if agent.Head().Fact(path).State() == testenv.FactStateExists {
					agent.Delete(path, fmt.Sprintf("step%d delete", step))
				}
			}
		}
	}
}

// ── P2 ────────────────────────────────────────────────────────────────────

// TestProperty_HistoryLinearityUnderConcurrentWrites generates N
// concurrent writers (N ∈ [2, 20]) each writing a unique file. After
// the parallel batch completes, the branch must show exactly N new
// commits, exactly N distinct facts, and pass deep Verify. If any
// writer's commit went missing or a merge-commit crept in, this
// fires.
func TestProperty_HistoryLinearityUnderConcurrentWrites(t *testing.T) {
	proptestSkip(t)
	t.Log("P2: N concurrent writers, all commits land linearly")
	seed := proptestSeed(t)

	const iterations = 50
	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		n := 2 + gen.Intn(19) // [2, 20]
		sb := testenv.NewStoryboard(t)
		repo := sb.Repo("alpha")
		agent := repo.Branch("agent/test")

		startCount := agent.CommitCount()
		sb.Parallel(n, func(i int) {
			path := fmt.Sprintf("kb/item%03d.md", i)
			content := testenv.Fact("v").Body(fmt.Sprintf("body-%d", i)).Build()
			repo.Instance().WithRead(func(svc *store.Service) {
				_, err := svc.Facts().WriteFact(context.Background(), "agent/test", path, content,
					fmt.Sprintf("w%d", i), "test")
				require.NoError(t, err)
			})
		})

		require.Equal(t, startCount+n, agent.CommitCount(),
			"iter %d: N=%d concurrent writers must produce N commits", iter, n)
		agent.MustHaveFactCount(n)
		repo.MustVerify()
	}
}

// ── P3 ────────────────────────────────────────────────────────────────────

// TestProperty_MergeIsIdempotent: given a random dst + random src
// state, running MergeFrom with the same strategy a second time must
// not advance dst (the strategies are deterministic and the merge
// already saw every src commit on the first call). The result state
// after the second merge equals the result state after the first.
func TestProperty_MergeIsIdempotent(t *testing.T) {
	proptestSkip(t)
	t.Log("P3: random state + merge twice with same strategy; second merge is a no-op")
	seed := proptestSeed(t)

	const iterations = 50
	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md", "kb/d.md"}
	strategies := []store.ConflictStrategy{store.StrategyLocalWins, store.StrategyRemoteWins}

	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		sb := testenv.NewStoryboard(t)
		repo := sb.Repo("alpha")
		dst := repo.Branch("main")

		// Seed a shared base.
		for _, p := range paths {
			dst.Write(p, testenv.Fact("base").Body("base"), "base "+p)
		}
		src := repo.BranchFrom("src", "main")

		// Apply random divergent ops on both branches.
		for range 5 {
			p := gen.Choose(paths)
			if gen.Bool() {
				dst.Write(p, testenv.Fact("d").Body(fmt.Sprintf("dst-%d", gen.Intn(1000))), "dst")
			}
			if gen.Bool() {
				src.Write(p, testenv.Fact("s").Body(fmt.Sprintf("src-%d", gen.Intn(1000))), "src")
			}
		}

		strat := strategies[gen.Intn(len(strategies))]
		dst.MergeFrom(src, strat)
		afterFirst := dst.CommitCount()
		firstPaths := snapshotPaths(t, repo, dst.Name())

		dst.MergeFrom(src, strat)
		afterSecond := dst.CommitCount()
		secondPaths := snapshotPaths(t, repo, dst.Name())

		require.Equal(t, afterFirst, afterSecond,
			"iter %d strat=%s: second merge must be a no-op", iter, strat)
		require.Equal(t, firstPaths, secondPaths,
			"iter %d strat=%s: second merge must leave fact set unchanged", iter, strat)
		repo.MustVerify()
	}
}

// ── P4 ────────────────────────────────────────────────────────────────────

// TestProperty_PushSyncRoundTripPreservesContent: write N random
// facts to agent A's agent branch, push to a shared bare remote,
// promote agent → main on the remote, then a fresh agent B syncs
// and must see exactly the same fact set with identical content.
func TestProperty_PushSyncRoundTripPreservesContent(t *testing.T) {
	proptestSkip(t)
	t.Log("P4: random facts on A's agent, push, promote to main, B syncs, B sees the same set with same content")
	seed := proptestSeed(t)

	const iterations = 30
	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		n := 3 + gen.Intn(8) // [3, 10]
		sb := testenv.NewStoryboard(t)
		remote := sb.BareRemote("origin")

		a := sb.Repo("a").Connect(remote)
		aAgent := a.Branch("agent/test")

		want := map[string]string{}
		for i := range n {
			path := fmt.Sprintf("kb/p4-%d.md", i)
			body := fmt.Sprintf("body-%d-%d", iter, gen.Intn(1000))
			aAgent.Write(path, testenv.Fact("p4").Body(body), fmt.Sprintf("add %d", i))
			want[path] = body
		}
		aAgent.Push()

		// Promote A's agent into main on the remote so B's Sync (which
		// pulls origin/main and replays the agent on top) sees A's facts.
		remote.MergeIntoMain("agent/test", "promote A's agent to main")

		b := sb.Repo("b").Connect(remote)
		bAgent := b.Branch("agent/test")
		bAgent.Sync()

		require.Equal(t, len(want), bAgent.FactCount(),
			"iter %d: B must see the same number of facts as A pushed", iter)
		for path, body := range want {
			bAgent.Head().Fact(path).Body().MustContain(body)
		}
		a.MustVerify()
		b.MustVerify()
	}
}

// ── P5 ────────────────────────────────────────────────────────────────────

// TestProperty_TemporalRefInvariant: write a fact B, write a fact A
// that refs B (pinning commit c1), then randomly mutate B several
// times. At every snapshot along the way, A@c1.FollowRef("kb/b.md")
// must return the B-content that was live at c1, not the later
// values. This is the C-category invariant at scale.
func TestProperty_TemporalRefInvariant(t *testing.T) {
	proptestSkip(t)
	t.Log("P5: A refs B at c1; mutate B; A@c1 FollowRef always returns c1-era B")
	seed := proptestSeed(t)

	const iterations = 30
	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("agent/test")

		// B starts with a specific body. A gets written right after,
		// pinning c1.
		b0 := fmt.Sprintf("b-at-c1-%d", gen.Intn(1000))
		agent.Write("kb/b.md", testenv.Fact("b").Body(b0), "init b")
		c1 := agent.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"), "a refs b")

		// Mutate B several times with distinct bodies.
		nMutations := 2 + gen.Intn(5)
		for j := range nMutations {
			body := fmt.Sprintf("b-mut-%d-%d", j, gen.Intn(1000))
			agent.Update("kb/b.md", testenv.Fact("b").Body(body), fmt.Sprintf("mut %d", j))
			// FollowRef from c1 must still see b0, no matter how many
			// times B has been mutated since.
			c1.Fact("kb/a.md").FollowRef("kb/b.md").Body().MustContain(b0)
		}
	}
}

// ── P6 ────────────────────────────────────────────────────────────────────

// TestProperty_BranchIsolationUnderRandomOps drives random
// write/update/delete ops across a fixed set of branches and applies
// every op to both the real Storyboard and a testenv.StoreModel
// reference. At the end of each iteration every branch's real state
// must equal the model's branch state (fact count + per-path body
// equivalence).
func TestProperty_BranchIsolationUnderRandomOps(t *testing.T) {
	proptestSkip(t)
	t.Log("P6: random ops across branches, real state must equal model state")
	seed := proptestSeed(t)

	const iterations = 50
	const stepsPerIter = 30
	branchNames := []string{"agent/test", "feature/a", "feature/b"}
	paths := []string{"kb/a.md", "kb/b.md", "kb/c.md"}

	for iter := range iterations {
		gen := testenv.NewOpGen(seed + int64(iter))
		sb := testenv.NewStoryboard(t)
		repo := sb.Repo("alpha")
		model := testenv.NewStoreModel()

		// agent/test exists at boot; the others are children of agent/test.
		// Seed the model to match.
		model.CreateBranch("agent/test", "main")
		_ = repo.Branch("agent/test") // materialize DSL handle
		for _, name := range branchNames[1:] {
			repo.BranchFrom(name, "agent/test")
			model.CreateBranch(name, "agent/test")
		}

		for step := range stepsPerIter {
			branchName := gen.Choose(branchNames)
			b := repo.Branch(branchName)
			p := gen.Choose(paths)
			body := fmt.Sprintf("iter%d-step%d-%d", iter, step, gen.Intn(10000))
			op := gen.Choose([]string{"write", "update", "delete"})
			switch op {
			case "write", "update":
				b.Write(p, testenv.Fact("p6").Body(body), fmt.Sprintf("%s step%d", op, step))
				model.Write(branchName, p, body)
			case "delete":
				if b.Head().Fact(p).State() == testenv.FactStateExists {
					b.Delete(p, fmt.Sprintf("delete step%d", step))
					model.Delete(branchName, p)
				}
			}
		}

		// At end of iteration: every tracked branch's real state must
		// match the model's fact set exactly.
		for _, name := range branchNames {
			realBranch := repo.Branch(name)
			modelBranch := model.Branches[name]
			require.Equal(t, len(modelBranch.Facts), realBranch.FactCount(),
				"iter %d branch %s: FactCount mismatch", iter, name)
			for path, wantBody := range modelBranch.Facts {
				realBranch.Head().Fact(path).Body().MustContain(wantBody)
			}
		}
		repo.MustVerify()
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// snapshotPaths returns the sorted list of fact paths currently
// visible on the branch. Used by P3 to compare dst state across
// idempotent merges — for idempotence the path set must be identical
// and the commit count must be unchanged, which together imply
// nothing changed.
func snapshotPaths(t *testing.T, repo *testenv.RepoHandle, branchName string) []string {
	t.Helper()
	var paths []string
	repo.Instance().WithRead(func(svc *store.Service) {
		var err error
		paths, err = svc.Facts().ListAll(context.Background(), branchName)
		require.NoError(t, err)
	})
	sort.Strings(paths)
	return paths
}
