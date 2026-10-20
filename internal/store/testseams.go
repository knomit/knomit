package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
)

// Test seams for the store's white-box tests, which drive the derivation and
// commit-log internals through a full Service. Each panics outside a test
// binary (testing.Testing), like the fallback-signer hook, so production cannot
// lean on them. They add no behaviour: each is a one-line view of an internal.

func mustBeTest() {
	if !testing.Testing() {
		panic("store: test seam called outside a test binary")
	}
}

// IsDerivedForTest reports whether hash has derived path-change rows.
func (rh *repoHandler) IsDerivedForTest(ctx context.Context, hash string) (bool, error) {
	mustBeTest()
	return newDeriver(rh, ActiveTables).isDerived(ctx, rh.db, hash)
}

// DeriveReadWhileApplyingForTest reads commit h through a deriver that is
// applying, which must fail with ErrReadInApply.
func (rh *repoHandler) DeriveReadWhileApplyingForTest(h plumbing.Hash) error {
	mustBeTest()
	d := newDeriver(rh, ActiveTables)
	d.applying = true
	_, err := d.commit(h)
	return err
}

// DeriveHookForTest is the commit-log Derive hook of a fresh deriver for
// hashes, with nothing prepared.
func (rh *repoHandler) DeriveHookForTest(hashes []string) func(ctx context.Context, tx *sql.Tx, i int) error {
	mustBeTest()
	return newDeriver(rh, ActiveTables).hook(hashes, nil)
}

// IndexItemsForTest builds the commit-log items of commits with a fresh
// deriver.
func (rh *repoHandler) IndexItemsForTest(ctx context.Context, commits []*object.Commit, all bool) ([]storegit.CommitLogItem, error) {
	mustBeTest()
	return rh.indexItems(ctx, newDeriver(rh, ActiveTables), commits, all)
}

// CommitChangesForTest is a fresh deriver's tree diff of c.
func (rh *repoHandler) CommitChangesForTest(c *object.Commit) ([]storegit.CommitLogEntry, error) {
	mustBeTest()
	return newDeriver(rh, ActiveTables).commitChanges(c)
}

// RederiveDegradedForTest runs rederiveDegraded.
func (rh *repoHandler) RederiveDegradedForTest(ctx context.Context) (int, error) {
	mustBeTest()
	return rh.rederiveDegraded(ctx)
}

// ChangeListsForTest returns how many commits' tree diffs are held for
// indexing, and their total entry count.
func (rh *repoHandler) ChangeListsForTest() (commits, entries int) {
	mustBeTest()
	rh.changes.mu.Lock()
	defer rh.changes.mu.Unlock()
	return len(rh.changes.m), rh.changes.n
}

// ResetChangeListsForTest drops every held tree diff.
func (rh *repoHandler) ResetChangeListsForTest() {
	mustBeTest()
	rh.changes.mu.Lock()
	rh.changes.m, rh.changes.n = nil, 0
	rh.changes.mu.Unlock()
}

// BranchLockForTest returns branch's write lock, if one was ever taken.
func (rh *repoHandler) BranchLockForTest(branch string) (*sync.RWMutex, bool) {
	mustBeTest()
	v, ok := rh.branchMu.Load(branch)
	if !ok {
		return nil, false
	}
	return v.(*sync.RWMutex), true
}

// LockConfigForTest takes the remote-config WRITE lock, as configureRemote
// does across a refspec rewrite, and returns its unlock.
func (rh *repoHandler) LockConfigForTest() func() {
	mustBeTest()
	rh.configMu.Lock()
	return rh.configMu.Unlock
}

// ForgetBranchIDForTest drops name from the branch-id cache.
func (rh *repoHandler) ForgetBranchIDForTest(name string) {
	mustBeTest()
	rh.cache.remove(name)
}

// MetaGetForTest reads a meta row ("" when absent).
func MetaGetForTest(ctx context.Context, q storegit.CtxExecer, key string) (string, error) {
	mustBeTest()
	return metaGet(ctx, q, key)
}

// HasTrailersForTest reports whether ctx carries a Trailers value at all.
func HasTrailersForTest(ctx context.Context) bool {
	mustBeTest()
	_, set := ctx.Value(trailersCtxKey{}).(Trailers)
	return set
}

// TestFallbackAgentForTest is the installed test fallback agent-id function,
// nil when none is installed.
func TestFallbackAgentForTest() *func(branch string) string {
	mustBeTest()
	return testFallbackAgent.Load()
}
