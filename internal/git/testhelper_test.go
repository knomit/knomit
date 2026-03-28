package git_test

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"

	git "knomit/internal/git"
	storegit "knomit/internal/store/git"
)

// testBranch is the fixed branch used by all external tests.
const testBranch = "agent/test"

// newTestStorer creates an in-memory SQLite-backed storer with the git schema applied.
func newTestStorer(t *testing.T) *storegit.Storer {
	t.Helper()
	s, err := storegit.NewMemoryStorer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB().Close() })
	return s
}

// newTestStore initialises a fresh in-memory git store on testBranch.
func newTestStore(t *testing.T) *git.Store {
	t.Helper()
	store, err := git.InitWithStorer(newTestStorer(t), nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
