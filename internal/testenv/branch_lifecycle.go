package testenv

import (
	"context"

	"knomit/internal/store"
)

// WithRead delegates to the underlying RepoInstance.WithRead, exposing the
// store.Service to test code for direct API calls.
func (b *BranchHandle) WithRead(fn func(*store.Service)) {
	b.repo.ri.WithRead(fn)
}

// Drop removes the branch from the repo. Wraps store.Branches().DropBranch
// which (after production fix in commit ae8c684) deletes both the git ref
// and the SQLite branches / branch_facts / branch_commits rows. Also
// removes the branch from the parent RepoHandle's internal map so future
// Branch(name) calls return a fresh handle if the branch is recreated.
//
// Auto-verifies the repo after the drop.
func (b *BranchHandle) Drop() {
	t := b.repo.sb.t
	t.Helper()
	var err error
	b.repo.ri.WithRead(func(svc *store.Service) {
		err = svc.Branches().DropBranch(context.Background(), b.name)
	})
	if err != nil {
		t.Fatalf("Drop(%s): %v", b.name, err)
	}
	delete(b.repo.branches, b.name)
	if b.repo.sb.auto {
		AssertIntegrity(t, b.repo)
	}
}

// FactCount returns the number of .md files under kb/ at the branch's
// current HEAD. Uses Facts().ListAll via the production API and filters
// to kb/*.md paths, matching what Verify's facts-coherence check considers
// a "fact at HEAD".
func (b *BranchHandle) FactCount() int {
	t := b.repo.sb.t
	t.Helper()
	var paths []string
	var err error
	b.repo.ri.WithRead(func(svc *store.Service) {
		paths, err = svc.Facts().ListAll(context.Background(), b.name)
	})
	if err != nil {
		t.Fatalf("FactCount(%s): %v", b.name, err)
	}
	count := 0
	for _, p := range paths {
		if len(p) > 3 && p[:3] == "kb/" && p[len(p)-3:] == ".md" {
			count++
		}
	}
	return count
}

// MustHaveFactCount asserts the branch has exactly n facts under kb/ at HEAD.
func (b *BranchHandle) MustHaveFactCount(n int) {
	b.repo.sb.t.Helper()
	got := b.FactCount()
	if got != n {
		b.repo.sb.t.Fatalf("branch %s: FactCount=%d, want %d", b.name, got, n)
	}
}

// CommitCount returns the number of commits reachable from the branch's
// current HEAD, counted by walking first-parents. Used by concurrency
// tests to assert "N writers produced exactly N commits" without
// worrying about merge-commit second-parents.
func (b *BranchHandle) CommitCount() int {
	t := b.repo.sb.t
	t.Helper()
	var count int
	b.repo.ri.WithRead(func(svc *store.Service) {
		hash, err := svc.Branches().HeadCommit(context.Background(), b.name)
		if err != nil {
			t.Fatalf("CommitCount(%s): HeadCommit: %v", b.name, err)
		}
		// Walk first-parent chain from HEAD via Facts().Log on an empty
		// path. Empty path returns every commit on the branch regardless
		// of which file it touched.
		//
		// Actually: Facts().Log takes (ctx, branch, path) where path is
		// the required file filter. For total commit count we need a
		// different path. The production Log interface is path-scoped,
		// so instead we walk via the repo instance directly using the
		// underlying git repo. Simpler: count rows in commit_log via raw
		// SQL — every commit on the branch has at least one commit_log
		// row per path it touched, so SELECT COUNT(DISTINCT commit_hash)
		// FROM branch_commits WHERE branch_id = (...).
		db := svc.RawDBForTest()
		row := db.QueryRow(`
			SELECT COUNT(*)
			FROM branch_commits
			WHERE branch_id = (SELECT id FROM branches WHERE name = ?)
		`, b.name)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("CommitCount(%s): query branch_commits: %v", b.name, err)
		}
		_ = hash
	})
	return count
}

// MustHaveCommitCount asserts that the branch has exactly n commits
// visible on it (via branch_commits).
func (b *BranchHandle) MustHaveCommitCount(n int) {
	b.repo.sb.t.Helper()
	got := b.CommitCount()
	if got != n {
		b.repo.sb.t.Fatalf("branch %s: CommitCount=%d, want %d", b.name, got, n)
	}
}

// Log returns the production Log entries for a path on this branch.
// Wraps svc.Search().Log — the path-history view from commit_log.
func (b *BranchHandle) Log(path string) []store.LogEntry {
	t := b.repo.sb.t
	t.Helper()
	var entries []store.LogEntry
	var err error
	b.repo.ri.WithRead(func(svc *store.Service) {
		entries, err = svc.Search().Log(context.Background(), b.name, path)
	})
	if err != nil {
		t.Fatalf("Log(%s on %s): %v", path, b.name, err)
	}
	return entries
}
