package web

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/git"
	storegit "knomit/internal/store/git"
)

// testAgentBranch is the branch used in web tests.
const testAgentBranch = "agent/test"

// newWebTestStore initialises a fresh in-memory git store for web tests.
func newWebTestStore(t *testing.T) *git.Store {
	t.Helper()
	s, err := storegit.NewMemoryStorer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB().Close() })
	store, err := git.InitWithStorer(s, nil, testAgentBranch)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
