// Internal test helpers — package git for access to unexported fields.
package git

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"

	storegit "knomit/internal/store/git"
)

// newInternalTestStorer creates an in-memory SQLite-backed storer.
func newInternalTestStorer(t *testing.T) *storegit.Storer {
	t.Helper()
	s, err := storegit.NewMemoryStorer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB().Close() })
	return s
}

// newInternalTestStore initialises a fresh in-memory git store for internal tests.
func newInternalTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := InitWithStorer(newInternalTestStorer(t), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return store
}
