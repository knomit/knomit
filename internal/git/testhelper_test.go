package git_test

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"

	git "knomit/internal/git"
	storegit "knomit/internal/store/git"
)

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

// newTestStore initialises a fresh in-memory git store.
func newTestStore(t *testing.T) *git.Store {
	t.Helper()
	store, err := git.InitWithStorer(newTestStorer(t), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return store
}
