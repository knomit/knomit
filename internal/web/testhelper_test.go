package web

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/store"
)

// testAgentBranch is the branch used in web tests.
const testAgentBranch = "agent/test"

// newWebTestStore initialises a fresh in-memory git store for web tests.
func newWebTestStore(t *testing.T) *store.Service {
	t.Helper()
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	if err := svc.InitRepo(nil, testAgentBranch); err != nil {
		t.Fatal(err)
	}
	return svc
}
