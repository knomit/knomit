package storytests

import (
	"context"
	"errors"
	"testing"

	"knomit/internal/store"
	"knomit/internal/testenv"
)

// TestFallbackBeforeReadsIndex pins the fact-read fallback-before path to the
// INDEX resolver (resolveActiveCommitForPath), not a go-git committer-time
// walk. The behavior under test:
//
//   - fallback steps OVER retractions to the last added/modified version, so it
//     resolves the same version at the retract commit AND at any later commit
//     (the go-git walk used to 404 once the most recent file event was a
//     deletion);
//   - a path never written in the ancestry is ErrPathNotFound.
//
// This is what keeps fallback-before content consistent with /incoming and
// /outgoing, which resolve the effective commit the same way.
func TestFallbackBeforeReadsIndex(t *testing.T) {
	readBefore := func(t *testing.T, b *testenv.BranchHandle, path, before string) (store.ReadFactResult, error) {
		t.Helper()
		var (
			res store.ReadFactResult
			err error
		)
		b.WithRead(func(svc *store.Service) {
			res, err = svc.Facts().ReadFact(context.Background(), "main", path,
				&store.ReadFactOpts{BeforeCommit: before})
		})
		return res, err
	}

	t.Run("steps_over_retraction_at_and_after_the_delete", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		cAdd := agent.Write("kb/x.md", testenv.Fact("x"), "add x")
		cDel := agent.Delete("kb/x.md", "retract x")
		cLater := agent.Write("kb/unrelated.md", testenv.Fact("u"), "later, unrelated commit")

		// At the delete commit: fall back to the add.
		if res, err := readBefore(t, agent, "kb/x.md", cDel.Commit); err != nil {
			t.Fatalf("at delete commit: %v", err)
		} else if res.FromCommit != cAdd.Commit {
			t.Errorf("at delete commit: FromCommit=%s, want add %s", res.FromCommit, cAdd.Commit)
		}

		// At a commit AFTER the delete: still falls back to the add (the go-git
		// walk returned ErrPathNotFound here — the regression this guards).
		if res, err := readBefore(t, agent, "kb/x.md", cLater.Commit); err != nil {
			t.Fatalf("after delete commit: %v", err)
		} else if res.FromCommit != cAdd.Commit {
			t.Errorf("after delete commit: FromCommit=%s, want add %s", res.FromCommit, cAdd.Commit)
		}
	})

	t.Run("resolves_to_last_modify_not_original_add", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")

		agent.Write("kb/x.md", testenv.Fact("x"), "add x")
		cMod := agent.Write("kb/x.md", testenv.Fact("x v2"), "modify x")
		cDel := agent.Delete("kb/x.md", "retract x")

		if res, err := readBefore(t, agent, "kb/x.md", cDel.Commit); err != nil {
			t.Fatalf("%v", err)
		} else if res.FromCommit != cMod.Commit {
			t.Errorf("FromCommit=%s, want last modify %s", res.FromCommit, cMod.Commit)
		}
	})

	t.Run("never_written_is_not_found", func(t *testing.T) {
		sb := testenv.NewStoryboard(t)
		agent := sb.Repo("alpha").Branch("main")
		c := agent.Write("kb/x.md", testenv.Fact("x"), "add x")

		if _, err := readBefore(t, agent, "kb/never.md", c.Commit); !errors.Is(err, store.ErrPathNotFound) {
			t.Errorf("want ErrPathNotFound, got %v", err)
		}
	})
}
