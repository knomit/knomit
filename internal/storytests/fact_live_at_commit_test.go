package storytests

import (
	"context"
	"testing"

	"knomit/internal/store"
	"knomit/internal/testenv"
)

// TestFactLiveAtCommit exercises the delete-RESPECTING existence primitive
// used to gate the commit-anchored /incoming and /outgoing sub-resources.
//
// The invariant under test: as of a commit, a fact's liveness reflects the
// MOST RECENT event in the first-parent ancestry — a retraction wins over an
// earlier add. This is the opposite of resolveActiveCommitForPath (which steps
// over deletions to find the last navigable version).
func TestFactLiveAtCommit(t *testing.T) {
	live := func(t *testing.T, b *testenv.BranchHandle, path, commit string) bool {
		t.Helper()
		var (
			ok  bool
			err error
		)
		b.WithRead(func(svc *store.Service) {
			ok, err = svc.Search().FactLiveAtCommit(context.Background(), "main", path, commit)
		})
		if err != nil {
			t.Fatalf("FactLiveAtCommit(%s @ %s): %v", path, commit, err)
		}
		return ok
	}

	t.Run("added_then_retracted_is_gone_after_the_delete", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		cAdd := agent.Write("kb/x.md", testenv.Fact("x"), "add x")
		cDel := agent.Delete("kb/x.md", "retract x")
		cAfter := agent.Write("kb/unrelated.md", testenv.Fact("u"), "later, unrelated commit")

		if !live(t, agent, "kb/x.md", cAdd.Commit) {
			t.Error("should be live at the add commit")
		}
		if live(t, agent, "kb/x.md", cDel.Commit) {
			t.Error("should NOT be live at the delete commit (retracted)")
		}
		if live(t, agent, "kb/x.md", cAfter.Commit) {
			t.Error("should NOT be live at a commit after the retraction")
		}
	})

	t.Run("re_added_after_retraction_is_live_again", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		agent.Write("kb/x.md", testenv.Fact("x"), "add x")
		agent.Delete("kb/x.md", "retract x")
		cReadd := agent.Write("kb/x.md", testenv.Fact("x again"), "re-add x")

		if !live(t, agent, "kb/x.md", cReadd.Commit) {
			t.Error("should be live again at the re-add commit")
		}
	})

	t.Run("still_live_at_a_later_commit_when_never_retracted", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		agent.Write("kb/x.md", testenv.Fact("x"), "add x")
		cLater := agent.Write("kb/y.md", testenv.Fact("y"), "add unrelated y")

		if !live(t, agent, "kb/x.md", cLater.Commit) {
			t.Error("a never-retracted fact stays live at later commits")
		}
	})

	t.Run("never_written_is_not_live", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")
		c := agent.Write("kb/x.md", testenv.Fact("x"), "add x")

		if live(t, agent, "kb/never.md", c.Commit) {
			t.Error("a path never written in the ancestry is not live")
		}
	})
}
