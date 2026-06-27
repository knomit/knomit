// Category D — Replay (merge) strategies. These tests assert the
// ConflictStrategy semantics from the working.md blueprint:
//
//	"if new knowledge is downloaded
//	   - it is merged into the agent branch using a merging strategy
//	     - local wins or remote wins
//	   - if remote wins, any changes from main will apply on top of
//	     agent branch - including deletes"
//
// The original spec called this "Replay" because of an early naming
// convention in the codebase. The actual primitive that honors strategies
// AND produces real git merges is MergeBranch (refactored in commit
// f9ef0f9). The DSL exposes it as BranchHandle.MergeFrom.
//
// In the category-D vocabulary, "remote" = src (the branch providing
// updates) and "local" = dst (the branch being updated). LocalWins keeps
// dst's version on conflict; RemoteWins takes src's version.
package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/internal/testenv"
)

// ── D1 ────────────────────────────────────────────────────────────────────

// TestReplay_LocalWinsPrefersLocal: base X=v1, local X=v2, remote X=v3.
// MergeFrom(remote, LocalWins) keeps local's v2.
func TestReplay_LocalWinsPrefersLocal(t *testing.T) {
	t.Log("D1: base X=v1, dst X=v2, src X=v3, MergeFrom(LocalWins) → dst HEAD has v2")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/x.md", testenv.Fact("x").Body("v1"), "base v1")
	src := repo.BranchFrom("src", "main")

	dst.Update("kb/x.md", testenv.Fact("x").Body("v2 local"), "local v2")
	src.Update("kb/x.md", testenv.Fact("x").Body("v3 remote"), "remote v3")

	dst.MergeFrom(src, store.StrategyLocalWins)
	dst.Head().Fact("kb/x.md").Body().MustContain("v2 local")
}

// ── D2 ────────────────────────────────────────────────────────────────────

// TestReplay_RemoteWinsPrefersRemote: same setup as D1, RemoteWins
// strategy. The dst takes src's version.
func TestReplay_RemoteWinsPrefersRemote(t *testing.T) {
	t.Log("D2: base X=v1, dst X=v2, src X=v3, MergeFrom(RemoteWins) → dst HEAD has v3")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/x.md", testenv.Fact("x").Body("v1"), "base v1")
	src := repo.BranchFrom("src", "main")

	dst.Update("kb/x.md", testenv.Fact("x").Body("v2 local"), "local v2")
	src.Update("kb/x.md", testenv.Fact("x").Body("v3 remote"), "remote v3")

	dst.MergeFrom(src, store.StrategyRemoteWins)
	dst.Head().Fact("kb/x.md").Body().MustContain("v3 remote")
}

// ── D3 ────────────────────────────────────────────────────────────────────

// TestReplay_LocalWinsDoesNotDropOverwrittenHistory: D1 setup. After
// MergeFrom(LocalWins), src's "losing" v3 commit is still reachable in
// the merge commit's history (parent[1] = src's HEAD).
//
// The merge commit on dst has two parents: parent[0] = dst's pre-merge
// HEAD (v2), parent[1] = src's HEAD (v3). The losing version is reachable
// from the merge commit via parent[1], so AtCommit on src's commit hash
// returns v3.
func TestReplay_LocalWinsDoesNotDropOverwrittenHistory(t *testing.T) {
	t.Log("D3: D1 setup; after merge, src's overwritten v3 still reachable via AtCommit on src's commit hash")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/x.md", testenv.Fact("x").Body("v1"), "base v1")
	src := repo.BranchFrom("src", "main")

	dst.Update("kb/x.md", testenv.Fact("x").Body("v2 local"), "local v2")
	srcHead := src.Update("kb/x.md", testenv.Fact("x").Body("v3 remote"), "remote v3")

	dst.MergeFrom(src, store.StrategyLocalWins)

	// dst HEAD has v2 (local wins).
	dst.Head().Fact("kb/x.md").Body().MustContain("v2 local")
	// But the v3 commit is still reachable on src's branch via its hash.
	src.At(srcHead).Fact("kb/x.md").Body().MustContain("v3 remote")
}

// ── D4 ────────────────────────────────────────────────────────────────────

// TestReplay_RemoteWinsAppliesDeletes: base has X. src deletes X. dst
// untouched. MergeFrom(RemoteWins) propagates the delete to dst.
func TestReplay_RemoteWinsAppliesDeletes(t *testing.T) {
	t.Log("D4: src deletes X, dst untouched, MergeFrom(RemoteWins) propagates the delete to dst")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/x.md", testenv.Fact("x"), "init x")
	src := repo.BranchFrom("src", "main")

	src.Delete("kb/x.md", "src deletes x")

	dst.MergeFrom(src, store.StrategyRemoteWins)
	dst.Head().Fact("kb/x.md").MustNotExist()
}

// ── D5 ────────────────────────────────────────────────────────────────────

// TestReplay_LocalWinsIgnoresRemoteDeletes: dst modified X, src deleted
// X. LocalWins keeps dst's modification (the conflict resolution favors
// dst, so the delete from src is skipped).
func TestReplay_LocalWinsIgnoresRemoteDeletes(t *testing.T) {
	t.Log("D5: dst modifies X, src deletes X, MergeFrom(LocalWins) keeps dst's modification")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/x.md", testenv.Fact("x").Body("original"), "init x")
	src := repo.BranchFrom("src", "main")

	dst.Update("kb/x.md", testenv.Fact("x").Body("dst modification"), "dst update")
	src.Delete("kb/x.md", "src deletes x")

	dst.MergeFrom(src, store.StrategyLocalWins)
	// dst's modification survives.
	dst.Head().Fact("kb/x.md").Body().MustContain("dst modification")
}

// ── D6 ────────────────────────────────────────────────────────────────────

// TestReplay_DisjointChangesBothApply: dst writes A on its branch, src
// writes B on its branch (after diverging). Neither touched the same
// file. Both strategies merge to a result containing both A and B.
func TestReplay_DisjointChangesBothApply(t *testing.T) {
	t.Log("D6: dst adds A, src adds B (disjoint), both strategies produce a result with both facts")
	for _, strat := range []store.ConflictStrategy{store.StrategyLocalWins, store.StrategyRemoteWins} {
		t.Run(string(strat), func(sub *testing.T) {
			sb := testenv.NewStoryboard(sub)
			repo := sb.Repo("alpha")
			dst := repo.Branch("main")
			src := repo.BranchFrom("src", "main")

			dst.Write("kb/a.md", testenv.Fact("a"), "dst adds a")
			src.Write("kb/b.md", testenv.Fact("b"), "src adds b")

			dst.MergeFrom(src, strat)
			dst.Head().Fact("kb/a.md").MustExist()
			dst.Head().Fact("kb/b.md").MustExist()
		})
	}
}

// ── D7 ────────────────────────────────────────────────────────────────────

// TestReplay_ResultIsIntegral asserts that after a merge of any
// shape, the repo passes Deep Verify. This is redundant with the auto-
// verify that runs after every DSL mutation, but it pins the explicit
// expectation that merges produce an integrity-clean result.
func TestReplay_ResultIsIntegral(t *testing.T) {
	t.Log("D7: after a non-trivial merge, VerifyWith Deep:true is strictly clean")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	dst := repo.Branch("main")
	dst.Write("kb/a.md", testenv.Fact("a"), "init a")
	src := repo.BranchFrom("src", "main")

	dst.Write("kb/d1.md", testenv.Fact("d1"), "dst adds d1")
	dst.Update("kb/a.md", testenv.Fact("a").Confidence(0.8), "dst updates a")
	src.Write("kb/s1.md", testenv.Fact("s1"), "src adds s1")
	src.Update("kb/a.md", testenv.Fact("a").Confidence(0.6), "src updates a")

	dst.MergeFrom(src, store.StrategyLocalWins)

	rep := repo.VerifyWith(store.VerifyOpts{Deep: true})
	require.True(t, rep.IsStrictlyClean(), "post-merge repo must be strictly clean: %v", rep.Issues)
}
