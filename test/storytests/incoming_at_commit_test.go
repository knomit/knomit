package storytests

import (
	"context"
	"testing"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// Tests for the design spec's "Test scenarios" §1-7, exercising the
// commit-anchored /incoming and /outgoing flows end-to-end.
func TestIncomingAtCommit_StoryScenarios(t *testing.T) {
	t.Run("1_unchanged_target_two_source_versions", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		cE := agent.Write("kb/e.md", testenv.Fact("e"), "init e")
		cD1 := agent.Write("kb/d.md", testenv.Fact("d").Refs("kb/e.md"), "d→e")
		cD2 := agent.Write("kb/d.md", testenv.Fact("d v2").Refs("kb/e.md"), "d v2→e")

		view := cE.Fact("kb/e.md").Incoming()
		view.MustHaveCount(2)
		view.MustHaveItem("kb/d.md", cD1.Commit)
		view.MustHaveItem("kb/d.md", cD2.Commit)
	})

	t.Run("2_target_supersession_isolates_lineage", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		cE1 := agent.Write("kb/e.md", testenv.Fact("e"), "init e")
		cD := agent.Write("kb/d.md", testenv.Fact("d").Refs("kb/e.md"), "d→e")
		cE2 := agent.Write("kb/e.md", testenv.Fact("e v2"), "update e")
		cF := agent.Write("kb/f.md", testenv.Fact("f").Refs("kb/e.md"), "f→e")

		cE1.Fact("kb/e.md").Incoming().MustHaveOnly("kb/d.md", cD.Commit)
		cE2.Fact("kb/e.md").Incoming().MustHaveOnly("kb/f.md", cF.Commit)
	})

	t.Run("3_branch_isolation", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		repo := sb.Repo("alpha")
		bA := repo.BranchFrom("agent/a", "main")
		bB := repo.BranchFrom("agent/b", "main")

		cEa := bA.Write("kb/e.md", testenv.Fact("e on A"), "init e on A")
		cDa := bA.Write("kb/d.md", testenv.Fact("d on A").Refs("kb/e.md"), "d→e on A")

		cEb := bB.Write("kb/e.md", testenv.Fact("e on B"), "init e on B")
		cDb := bB.Write("kb/d.md", testenv.Fact("d on B").Refs("kb/e.md"), "d→e on B")

		cEa.Fact("kb/e.md").Incoming().MustHaveOnly("kb/d.md", cDa.Commit)
		cEb.Fact("kb/e.md").Incoming().MustHaveOnly("kb/d.md", cDb.Commit)
	})

	t.Run("4_ref_to_nonexistent_path_no_edge", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")
		cD := agent.Write("kb/d.md", testenv.Fact("d").Refs("kb/never.md"), "d→never")
		cD.Fact("kb/d.md").Outgoing().MustHaveCount(0)
	})

	t.Run("5_intra_commit_refs_resolve_to_same_commit", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		// Both A.md and B.md in the same git commit; A.md ref's B.md.
		snap := agent.Batch("intra-commit refs", func(w *testenv.BatchWriter) {
			w.Write("kb/a.md", testenv.Fact("a").Refs("kb/b.md"))
			w.Write("kb/b.md", testenv.Fact("b"))
		})

		// A's outgoing ref to B should resolve to the same batch commit.
		snap.Fact("kb/a.md").Outgoing().MustHaveOnly("kb/b.md", snap.Commit)

		// B's incoming should likewise show A pointing from the same commit.
		snap.Fact("kb/b.md").Incoming().MustHaveOnly("kb/a.md", snap.Commit)
	})

	t.Run("6_head_parity", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		_ = agent.Write("kb/e.md", testenv.Fact("e"), "init e")
		cD := agent.Write("kb/d.md", testenv.Fact("d").Refs("kb/e.md"), "d→e")

		// HEAD-side ExplainFact returns the same lineage as commit-anchored
		// /incoming at HEAD-active commit for kb/e.md.
		var headIncoming []store.RefSummary
		var explainErr error
		agent.WithRead(func(svc *store.Service) {
			result, err := svc.Search().ExplainFact(context.Background(), "main", "kb/e.md")
			if err != nil {
				explainErr = err
				return
			}
			headIncoming = result.Incoming
		})
		if explainErr != nil {
			t.Fatalf("ExplainFact: %v", explainErr)
		}
		if len(headIncoming) != 1 {
			t.Fatalf("expected 1 incoming via ExplainFact, got %d: %+v", len(headIncoming), headIncoming)
		}
		if headIncoming[0].Path != "kb/d.md" || headIncoming[0].Commit != cD.Commit {
			t.Fatalf("ExplainFact returned (%s, %s), want (kb/d.md, %s)",
				headIncoming[0].Path, headIncoming[0].Commit, cD.Commit)
		}
	})

	t.Run("7_multi_edge_same_pair", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		cE := agent.Write("kb/e.md", testenv.Fact("e"), "init e")
		cD1 := agent.Write("kb/d.md", testenv.Fact("d").Refs("kb/e.md"), "d→e")
		cD2 := agent.Write("kb/d.md", testenv.Fact("d v2").Refs("kb/e.md"), "d v2→e")

		view := cE.Fact("kb/e.md").Incoming()
		view.MustHaveCount(2)
		view.MustHaveItem("kb/d.md", cD1.Commit)
		view.MustHaveItem("kb/d.md", cD2.Commit)
	})

	// Scenario 8 — covers the topological vs committed_at divergence
	// flagged in .claude/plans/2026-05-01-topological-ordering-followup.md.
	//
	// Setup:
	//   main:  cM1 (add E v1) → cM2 (modify E to v2-main)
	//   agent: forked from cM1; cB1 modifies E to v2-branch AND adds F.
	//
	// main then merges agent with StrategyLocalWins. The merge:
	//   - For E: both sides changed it relative to base (cM1) → conflict;
	//     LocalWins keeps main's version (E stays at cM2's content). The
	//     merge commit's diff against parent[0]=cM2 has NO row for E in
	//     commit_log, because E didn't change relative to cM2.
	//   - For F: only src added → applied. Merge commit DOES change tree.
	//
	// After merge, main's branch_commits includes cB1. A new fact G
	// written on main with refs=[kb/e.md] must resolve target_commit to
	// cM2 (main's first-parent active version of E), NOT cB1 (which is
	// reachable but is on the merged-in branch's history, not main's
	// first-parent line).
	//
	// committed_at-based resolver picks cB1 (highest committed_at among
	// commit_log rows touching E in branch_commits(main)). The
	// topological resolver picks cM2 by walking M → cM2 along first-parent.
	t.Run("8_merge_with_local_wins_topological_resolution", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		repo := sb.Repo("alpha")
		mainB := repo.Branch("main")

		_ = mainB.Write("kb/e.md", testenv.Fact("e v1"), "init e")
		// Fork agent/b from main NOW (at cM1), before main makes its
		// local change to E. agent/b will then diverge.
		fb := repo.BranchFrom("agent/b", "main")

		cM2 := mainB.Write("kb/e.md", testenv.Fact("e v2-main"), "modify e on main")

		// On agent/b: modify E (will conflict with main) AND add F (no
		// conflict, ensures merge isn't a no-op).
		_ = fb.Batch("modify e + add f on agent/b", func(w *testenv.BatchWriter) {
			w.Write("kb/e.md", testenv.Fact("e v2-branch"))
			w.Write("kb/f.md", testenv.Fact("f"))
		})

		// Merge agent/b into main with StrategyLocalWins. Conflict on E
		// resolves in main's favour (E stays at cM2 content); F is added.
		_ = mainB.MergeFrom(fb, store.StrategyLocalWins)

		// Write G on main referencing E. Topologically-correct target is
		// cM2 (main's active E along first-parent line).
		cG := mainB.Write("kb/g.md", testenv.Fact("g").Refs("kb/e.md"), "g→e")

		// G's outgoing edge for kb/e.md must target cM2 — NOT cB1
		// (committed_at would pick cB1 because it's wall-clock-newer and
		// reachable on main post-merge).
		out := cG.Fact("kb/g.md").Outgoing()
		out.MustHaveOnly("kb/e.md", cM2.Commit)
	})
}
