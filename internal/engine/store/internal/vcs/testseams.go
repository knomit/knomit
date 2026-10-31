package vcs

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/sqlite"
	"knomit/internal/engine/store/model"
)

// Test seams for the parent store's white-box tests, which drive the
// derivation and commit-log internals through a full Service and so cannot
// live in this package. Each panics outside a test binary (testing.Testing),
// like the fallback-signer hook, so production cannot lean on them; the Go
// internal rule already keeps them out of reach of anything outside
// internal/engine/store. They add no behaviour: each is a one-line view of an
// internal. The exported test hooks and counters (DeriveObjectHook, SwapHook,
// BranchTipHook, OpenHistoryHook, CommitDiffs, PayloadsComputed,
// LastSwapDuration) stay beside the code that reads them.

func mustBeTest() {
	if !testing.Testing() {
		panic("vcs: test seam called outside a test binary")
	}
}

// serviceRepo maps a *store.Service, passed as any because this package cannot
// import the store, to the *Repo it was built on. The store registers it from
// its init (RegisterServiceRepo); storetest reads it through
// ServiceRepoForTest. It is the only way a test outside package store reaches
// a vcs seam without a Service method or an exported accessor, so the store
// carries no test-only method and no value of this package's type crosses the
// internal/ rule in production code.
var serviceRepo func(svc any) *Repo

// RegisterServiceRepo installs the function ServiceRepoForTest uses. The
// store calls it once, from its init. A second registration panics: two
// registrations mean two stores claiming the same hook.
func RegisterServiceRepo(f func(svc any) *Repo) {
	if serviceRepo != nil {
		panic("vcs: RegisterServiceRepo called twice")
	}
	serviceRepo = f
}

// ServiceRepoForTest is the *Repo of svc, which must be the *store.Service the
// registered function accepts. Test-only by contract: it panics outside a test
// binary (mustBeTest). It is the getter behind every storetest function that
// needs the vcs handle.
func ServiceRepoForTest(svc any) *Repo {
	mustBeTest()
	if serviceRepo == nil {
		panic("vcs: ServiceRepoForTest called before the store registered its Service")
	}
	return serviceRepo(svc)
}

// IsDerivedForTest reports whether hash has derived path-change rows.
func (rh *Repo) IsDerivedForTest(ctx context.Context, hash string) (bool, error) {
	mustBeTest()
	return newDeriver(rh, ActiveTables).isDerived(ctx, rh.db, hash)
}

// DeriveReadWhileApplyingForTest reads commit h through a deriver that is
// applying, which must fail with ErrReadInApply.
func (rh *Repo) DeriveReadWhileApplyingForTest(h plumbing.Hash) error {
	mustBeTest()
	d := newDeriver(rh, ActiveTables)
	d.applying = true
	_, err := d.commit(h)
	return err
}

// DeriveHookForTest is the commit-log Derive hook of a fresh deriver for
// hashes, with nothing prepared.
func (rh *Repo) DeriveHookForTest(hashes []string) func(ctx context.Context, tx *sql.Tx, i int) error {
	mustBeTest()
	return newDeriver(rh, ActiveTables).hook(hashes, nil)
}

// IndexItemsForTest builds the commit-log items of commits with a fresh
// deriver.
func (rh *Repo) IndexItemsForTest(ctx context.Context, commits []*object.Commit, all bool) ([]storegit.CommitLogItem, error) {
	mustBeTest()
	return rh.indexItems(ctx, newDeriver(rh, ActiveTables), commits, all)
}

// CommitChangesForTest is a fresh deriver's tree diff of c.
func (rh *Repo) CommitChangesForTest(c *object.Commit) ([]storegit.CommitLogEntry, error) {
	mustBeTest()
	return newDeriver(rh, ActiveTables).commitChanges(c)
}

// RederiveDegradedForTest runs rederiveDegraded.
func (rh *Repo) RederiveDegradedForTest(ctx context.Context) (int, error) {
	mustBeTest()
	return rh.rederiveDegraded(ctx)
}

// ChangeListsForTest returns how many commits' tree diffs are held for
// indexing, and their total entry count.
func (rh *Repo) ChangeListsForTest() (commits, entries int) {
	mustBeTest()
	rh.changes.mu.Lock()
	defer rh.changes.mu.Unlock()
	return len(rh.changes.m), rh.changes.n
}

// ResetChangeListsForTest drops every held tree diff.
func (rh *Repo) ResetChangeListsForTest() {
	mustBeTest()
	rh.changes.mu.Lock()
	rh.changes.m, rh.changes.n = nil, 0
	rh.changes.mu.Unlock()
}

// BranchLockForTest returns branch's write lock, if one was ever taken.
func (rh *Repo) BranchLockForTest(branch string) (*sync.RWMutex, bool) {
	mustBeTest()
	v, ok := rh.branchMu.Load(branch)
	if !ok {
		return nil, false
	}
	return v.(*sync.RWMutex), true
}

// LockConfigForTest takes the remote-config WRITE lock, as ConfigureRemote
// does across a refspec rewrite, and returns its unlock.
func (rh *Repo) LockConfigForTest() func() {
	mustBeTest()
	rh.configMu.Lock()
	return rh.configMu.Unlock
}

// ForgetBranchIDForTest drops name from the branch-id cache.
func (rh *Repo) ForgetBranchIDForTest(name string) {
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
	return model.HasTrailers(ctx)
}

// TestFallbackAgentForTest is the installed test fallback agent-id function,
// nil when none is installed.
func TestFallbackAgentForTest() *func(branch string) string {
	mustBeTest()
	return testFallbackAgent.Load()
}

// SetRefForTest moves (or creates) a git reference WITHOUT a commit and without
// NotifyCommit — for tests in sibling packages that need a ref shape the write
// path never produces on demand: an agent branch rewound so its old head is no
// longer an ancestor (the replay case), or main moved to a commit as if the
// acceptance gate had advanced it.
//
// Test-only by contract: it panics outside a test binary (mustBeTest), so no
// production path can move a ref behind the store's back. Tests outside the
// store reach it through storetest.SetRef.
func (rh *Repo) SetRefForTest(name, hash string) error {
	mustBeTest()
	if !plumbing.IsHash(hash) {
		return fmt.Errorf("SetRefForTest: %q is not a commit hash", hash)
	}
	return rh.Storer().SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash(hash)))
}

// DeleteRefForTest removes a git reference WITHOUT NotifyCommit — for a test
// that needs a repo with no upstream branch (F07 PR 5: no main means no repo
// recipe tier). Test-only by contract (see SetRefForTest).
func (rh *Repo) DeleteRefForTest(name string) error {
	mustBeTest()
	return rh.Storer().RemoveReference(plumbing.ReferenceName(name))
}

// CommitFilesForTest commits files onto branch as ONE signed commit, moving
// the ref but skipping NotifyCommit — no commit_log row, no index sync, no
// observer. It exists for a test that needs a very large advance (thousands of
// fact paths) without paying the index's cost for it: the dispatcher's own
// work is what such a test measures. Test-only by contract (see
// SetRefForTest). Returns the commit hash.
//
// The trees are built in ONE pass (testingTree): each tree object the commit
// needs is written once. Applying the paths one at a time with BuildTree, as
// the write path does for its handful of files, rewrites every tree on the
// path for every file: about five tree objects per file, 60,000 loose objects
// for the 12,000-file advance of TestDispatch_Tx1Bounded, which cost that test
// ~15 s on the Windows runner. The resulting tree is the same object either
// way (TestTestingCommitFiles_TreeMatchesPerPathBuild).
func (rh *Repo) CommitFilesForTest(branch string, files map[string]string, message string) (string, error) {
	mustBeTest()
	ctx := context.Background()
	parent, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	parentCommit, err := rh.Repository().CommitObject(parent)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent: %w", err)
	}
	tree, err := parentCommit.Tree()
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent tree: %w", err)
	}
	root, err := testingTree(rh.Storer(), tree, files)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: %w", err)
	}
	signer, err := rh.CommitSigner()
	if err != nil {
		return "", err
	}
	author, committer, err := rh.CommitSigs(ctx, branch, "learn")
	if err != nil {
		return "", err
	}
	c := &object.Commit{
		Author:       author,
		Committer:    committer,
		Message:      message,
		TreeHash:     root,
		ParentHashes: []plumbing.Hash{parent},
	}
	h, err := StoreCommit(rh.Storer(), signer, c)
	if err != nil {
		return "", err
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)); err != nil {
		return "", err
	}
	return h.String(), nil
}

// FetchRefspecsForTest returns the fetch refspecs configured on the named git
// remote, or nil when the remote (or the repository) is absent. EXISTS ONLY so
// tests in other packages can assert that an origin change actually rewrote the
// git config — the refspec is what decides which branch the next Sync fetches,
// and it is otherwise invisible from outside this package (go-git's config lives
// in the SQLite-backed storer, not a .git/config file on disk).
//
// Without it, deleting the ConfigureRemote call from an upstream-change path
// leaves the stored branch correct and the fetch silently tracking the OLD
// branch — see TestPatchOriginUpstream_PersistsToControlDB.
func (rh *Repo) FetchRefspecsForTest(remote string) []string {
	mustBeTest()
	if rh.Repository() == nil {
		return nil
	}
	cfg, err := rh.Repository().Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		out = append(out, string(rs))
	}
	return out
}

// RemoteURLsForTest returns the URLs configured on the named git remote, or nil
// when the remote (or the repository) is absent. EXISTS ONLY as the URL half of
// FetchRefspecsForTest: an origin write that re-points the git remote and then
// fails to make that durable has to be observed as a URL, because the refspecs
// are unchanged when only the url moved.
//
// See TestSetOrigin_FailedPersistRestoresTheGitRemote — without this, a repo
// left fetching a url nothing records looks identical to one that was never
// touched.
func (rh *Repo) RemoteURLsForTest(remote string) []string {
	mustBeTest()
	if rh.Repository() == nil {
		return nil
	}
	cfg, err := rh.Repository().Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	return append([]string(nil), rc.URLs...)
}

// RawSymlinkForTest commits a SYMLINK entry (mode 120000) at path, case
// kept, whose blob is target — what `git add` of a symlink records. EXISTS
// ONLY for tests of how readers treat a symlink under .knomit/ (never
// followed, read as absent); no writer in knomit can create one.
func (rh *Repo) RawSymlinkForTest(ctx context.Context, branch, path, target, message string) (string, error) {
	mustBeTest()
	return rh.RawEntryForTest(ctx, branch, path, target, message, filemode.Symlink)
}

// RawEntryForTest commits one entry of the given mode at path, case kept,
// whose blob is content: RawSymlinkForTest's core, also used by the parent's
// own tests for an executable entry.
func (rh *Repo) RawEntryForTest(ctx context.Context, branch, path, content, message string, mode filemode.FileMode) (string, error) {
	mustBeTest()
	target := content
	unlock := rh.LockBranch(branch)
	defer unlock()
	head, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	tip, err := rh.Repository().CommitObject(head)
	if err != nil {
		return "", err
	}
	root, err := tip.Tree()
	if err != nil {
		return "", err
	}
	blob, err := WriteBlobToStore(rh.Storer(), []byte(target))
	if err != nil {
		return "", err
	}
	rootHash, err := BuildTreeMode(rh.Storer(), root, path, mode, blob)
	if err != nil {
		return "", err
	}
	c := &object.Commit{Author: tip.Author, Committer: tip.Committer, Message: message,
		TreeHash: rootHash, ParentHashes: []plumbing.Hash{head}}
	obj := rh.Storer().NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return "", err
	}
	h, err := rh.Storer().SetEncodedObject(obj)
	if err != nil {
		return "", err
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)); err != nil {
		return "", err
	}
	return h.String(), nil
}

// testingDir is one directory of a TestingCommitFiles batch: its files (name
// to content) and its subdirectories.
type testingDir struct {
	files map[string]string
	dirs  map[string]*testingDir
}

// testingTree writes the tree that results from adding files to existing (nil
// for an empty tree), writing each blob and each changed tree exactly once,
// and returns its hash. Entries of existing that the batch does not touch are
// kept; a batch path replaces an entry of the same name, as BuildTree does
// (a file over a directory, or a directory over a file).
func testingTree(s *storegit.Storer, existing *object.Tree, files map[string]string) (plumbing.Hash, error) {
	top := &testingDir{}
	for p, content := range files {
		parts := strings.Split(p, "/")
		d := top
		for _, name := range parts[:len(parts)-1] {
			if _, clash := d.files[name]; clash {
				return plumbing.ZeroHash, fmt.Errorf("tree %q: %q is both a file and a directory in the batch", p, name)
			}
			if d.dirs == nil {
				d.dirs = map[string]*testingDir{}
			}
			if d.dirs[name] == nil {
				d.dirs[name] = &testingDir{}
			}
			d = d.dirs[name]
		}
		leaf := parts[len(parts)-1]
		if _, clash := d.dirs[leaf]; clash {
			return plumbing.ZeroHash, fmt.Errorf("tree %q: %q is both a file and a directory in the batch", p, leaf)
		}
		if d.files == nil {
			d.files = map[string]string{}
		}
		d.files[leaf] = content
	}
	return top.write(s, existing)
}

func (d *testingDir) write(s *storegit.Storer, existing *object.Tree) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	if existing != nil {
		for _, e := range existing.Entries {
			_, f := d.files[e.Name]
			_, sub := d.dirs[e.Name]
			if !f && !sub {
				entries = append(entries, e)
			}
		}
	}
	for name, content := range d.files {
		h, err := WriteBlobToStore(s, []byte(content))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	for name, sub := range d.dirs {
		var prev *object.Tree
		if existing != nil {
			for _, e := range existing.Entries {
				if e.Name == name && e.Mode == filemode.Dir {
					t, err := object.GetTree(s, e.Hash)
					if err != nil {
						return plumbing.ZeroHash, fmt.Errorf("get subtree %q: %w", name, err)
					}
					prev = t
					break
				}
			}
		}
		h, err := sub.write(s, prev)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	sort.Sort(object.TreeEntrySorter(entries))
	obj := s.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	return s.SetEncodedObject(obj)
}

// gitPathReadHook, when set, is told the name of every per-path git read
// (ReadFileAtCommit, ReadBlobHashAtCommit, ResolveActiveCommitForPath) as it
// starts. nil in production: one atomic load per call.
var gitPathReadHook atomic.Pointer[func(name string)]

func noteGitPathRead(name string) {
	if h := gitPathReadHook.Load(); h != nil {
		(*h)(name)
	}
}

// SetGitPathReadHookForTest installs f as the per-path git read hook (nil
// removes it) and returns the restore. A test that must prove a read path
// decodes no git object per fact fails from f.
func SetGitPathReadHookForTest(f func(name string)) (restore func()) {
	mustBeTest()
	var p *func(string)
	if f != nil {
		p = &f
	}
	old := gitPathReadHook.Swap(p)
	return func() { gitPathReadHook.Store(old) }
}

// FirstParentChainCTEForTest is the first-parent walk the history readers
// ran before LineEvents (ResolveActiveCommitForPath, RevisionsBefore,
// FactLiveAtCommit, each with its own SELECT ... ORDER BY fpc.depth): it
// binds the start commit and yields fpc(commit_hash, depth) over
// commit_parents, parent_order = 0. Kept as the oracle the equivalence tests
// hold LineEvents to; no production reader uses it.
const FirstParentChainCTEForTest = `
	WITH RECURSIVE fpc(commit_hash, depth) AS (
	    SELECT ?, 0
	    UNION ALL
	    SELECT cp.parent_hash, fpc.depth + 1
	      FROM commit_parents cp
	      JOIN fpc ON cp.commit_hash = fpc.commit_hash AND cp.parent_order = 0
	)`

// LineEventsWalkForTest is LineEvents' fallback (the commit_parents walk)
// alone, so a test can hold it to the same answers as the commit_fp form.
func (rh *Repo) LineEventsWalkForTest(ctx context.Context, from string, lq LineQuery) ([]LineEvent, error) {
	mustBeTest()
	return lineEventsByWalk(ctx, sqlite.Conn(ctx, rh.db), from, lq)
}

// LineEventsJumpsCountingForTest is LineEvents' commit_fp form alone,
// counting the SQL statements it runs, so a test can bound its cost in
// statements rather than time. complete is false where LineEvents would
// fall back to the walk.
func (rh *Repo) LineEventsJumpsCountingForTest(ctx context.Context, from string, lq LineQuery) (evs []LineEvent, complete bool, statements int, err error) {
	mustBeTest()
	c := &countingExecer{CtxExecer: sqlite.Conn(ctx, rh.db)}
	evs, complete, err = lineEventsByJumps(ctx, c, from, lq)
	return evs, complete, c.n, err
}

type countingExecer struct {
	storegit.CtxExecer
	n int
}

func (c *countingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.n++
	return c.CtxExecer.ExecContext(ctx, query, args...)
}

func (c *countingExecer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.n++
	return c.CtxExecer.QueryContext(ctx, query, args...)
}

func (c *countingExecer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.n++
	return c.CtxExecer.QueryRowContext(ctx, query, args...)
}
