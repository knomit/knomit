package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// newChangesService opens a fresh store with "main" as the consensus branch and
// "agent/a" as the agent branch, both at the same root commit.
func newChangesService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "main", "agent/a"))
	return svc
}

func writeF(t *testing.T, svc *Service, branch, path string) string {
	t.Helper()
	r, err := svc.Facts().WriteFact(context.Background(), branch, path, testFactBody(path, 0.8, nil), "w "+path, "learn")
	require.NoError(t, err)
	return r.CommitHash
}

func modifyF(t *testing.T, svc *Service, branch, path string) string {
	t.Helper()
	r, err := svc.Facts().WriteFact(context.Background(), branch, path, testFactBody(path+" v2", 0.9, nil), "m "+path, "update")
	require.NoError(t, err)
	return r.CommitHash
}

func deleteF(t *testing.T, svc *Service, branch, path string) string {
	t.Helper()
	h, err := svc.Facts().DeleteFact(context.Background(), branch, path, "d "+path)
	require.NoError(t, err)
	return h
}

// restamp replaces the tip of branch with a commit carrying the SAME tree and
// message but the given committer/author time and (when non-nil) parents. It
// is how the tests build histories whose commit times lie — a child older than
// its parent, several commits in one second — which the writers never produce
// on their own because they stamp time.Now().
func restamp(t *testing.T, svc *Service, branch string, parents []plumbing.Hash, when time.Time) string {
	t.Helper()
	return restampAs(t, svc, branch, parents, when, "")
}

// restampAs is restamp with the message replaced when msg is non-empty.
func restampAs(t *testing.T, svc *Service, branch string, parents []plumbing.Hash, when time.Time, msg string) string {
	t.Helper()
	rh := svc.rh
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	tip, err := rh.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	if parents == nil {
		parents = tip.ParentHashes
	}
	sig := object.Signature{Name: "t", Email: "t+learn@agents.knomit.io", When: when}
	if msg == "" {
		msg = tip.Message
	}
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tip.TreeHash, ParentHashes: parents}
	obj := rh.repo.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := rh.repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	return h.String()
}

func changes(t *testing.T, svc *Service, branch string, q ChangesQuery) ChangesResult {
	t.Helper()
	res, err := svc.Facts().ChangesUnder(context.Background(), branch, q)
	require.NoError(t, err)
	return res
}

// Test 1: a deletion appears as a row, and only it.
func TestChangesUnder_DeletionAppears(t *testing.T) {
	svc := newChangesService(t)
	writeF(t, svc, "main", "kb/tasks/a/x.md")
	c0 := writeF(t, svc, "main", "kb/tasks/a/y.md")
	deleteF(t, svc, "main", "kb/tasks/a/x.md")

	res := changes(t, svc, "main", ChangesQuery{Since: c0, Prefix: "tasks/a"})
	require.Equal(t, []PathChange{{Path: "kb/tasks/a/x.md", Change: ChangeDeleted}}, res.Changes)
}

// Test 2: strictly after since — the state AT since is the baseline, so a path
// added at since and modified later is "modified", not "added".
func TestChangesUnder_StrictlyAfterSince(t *testing.T) {
	svc := newChangesService(t)
	c1 := writeF(t, svc, "main", "kb/tasks/a/a1.md")
	writeF(t, svc, "main", "kb/tasks/a/a2.md")
	c3 := modifyF(t, svc, "main", "kb/tasks/a/a1.md")

	res := changes(t, svc, "main", ChangesQuery{Since: c1, Prefix: "tasks/a"})
	require.Equal(t, []PathChange{
		{Path: "kb/tasks/a/a1.md", Change: ChangeModified},
		{Path: "kb/tasks/a/a2.md", Change: ChangeAdded},
	}, res.Changes)

	res = changes(t, svc, "main", ChangesQuery{Since: c3, Prefix: "tasks/a"})
	require.Empty(t, res.Changes)
	require.NotNil(t, res.Changes, "an empty page is [] on the wire, not null")
	require.Equal(t, c3, res.Head, "head is always returned, including on an empty page")

	// Omitted since = the folder as it is now, all "added".
	res = changes(t, svc, "main", ChangesQuery{Prefix: "tasks/a"})
	require.Equal(t, []PathChange{
		{Path: "kb/tasks/a/a1.md", Change: ChangeAdded},
		{Path: "kb/tasks/a/a2.md", Change: ChangeAdded},
	}, res.Changes)
}

// buildSkewedHistory writes the same four commits into svc with commit times
// that LIE: c2 is older than its parent c1, and c3/c4 share one second with
// c2. A reader that orders or filters by commit time gets this wrong; a
// reader of two trees cannot.
func buildSkewedHistory(t *testing.T, svc *Service) (since, head string) {
	t0 := time.Unix(1_000_000, 0).UTC()
	// Two instances of one repo share its root commit (a clone). Two fresh
	// stores do not — the root carries a per-repo nonce — so give both the same
	// root message and time.
	restampAs(t, svc, "main", nil, t0, "init: create knowledge base\n")
	writeF(t, svc, "main", "kb/tasks/a/p.md")
	since = restamp(t, svc, "main", nil, t0.Add(10*time.Second))
	writeF(t, svc, "main", "kb/tasks/a/q.md")
	restamp(t, svc, "main", nil, t0.Add(5*time.Second)) // older than its parent
	deleteF(t, svc, "main", "kb/tasks/a/p.md")
	restamp(t, svc, "main", nil, t0.Add(5*time.Second)) // same second
	writeF(t, svc, "main", "kb/tasks/a/r.md")
	head = restamp(t, svc, "main", nil, t0.Add(5*time.Second)) // same second
	return since, head
}

// Test 3: clock-free and identical across instances. Two stores built with the
// same (lying) history hold byte-identical commits, and must give
// byte-identical answers that include the commits whose time is BEFORE since.
func TestChangesUnder_ClockFreeAndIdenticalAcrossInstances(t *testing.T) {
	a, b := newChangesService(t), newChangesService(t)
	sinceA, headA := buildSkewedHistory(t, a)
	sinceB, headB := buildSkewedHistory(t, b)
	require.Equal(t, sinceA, sinceB, "fixture: both instances must hold the same commits")
	require.Equal(t, headA, headB, "fixture: both instances must hold the same commits")

	want := []PathChange{
		{Path: "kb/tasks/a/p.md", Change: ChangeDeleted},
		{Path: "kb/tasks/a/q.md", Change: ChangeAdded},
		{Path: "kb/tasks/a/r.md", Change: ChangeAdded},
	}
	ra := changes(t, a, "main", ChangesQuery{Since: sinceA, Prefix: "tasks/a"})
	rb := changes(t, b, "main", ChangesQuery{Since: sinceB, Prefix: "tasks/a"})
	require.Equal(t, want, ra.Changes, "every commit after since counts, whatever its timestamp says")

	ja, err := json.Marshal(ra)
	require.NoError(t, err)
	jb, err := json.Marshal(rb)
	require.NoError(t, err)
	require.Equal(t, string(ja), string(jb))
}

// Test 4: a page boundary, with head pinned across pages.
func TestChangesUnder_PageBoundaryPinsHead(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	since := writeF(t, svc, "main", "kb/other/seed.md")
	files := map[string]string{}
	for i := 0; i < 101; i++ {
		p := fmt.Sprintf("kb/tasks/a/t%03d.md", i)
		files[p] = testFactBody(p, 0.8, nil)
	}
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", files, nil, "bulk", "learn")
	require.NoError(t, err)

	p1 := changes(t, svc, "main", ChangesQuery{Since: since, Prefix: "tasks/a", Limit: 100})
	require.Len(t, p1.Changes, 100)
	require.True(t, p1.HasMore)
	last := p1.Changes[len(p1.Changes)-1].Path
	require.Equal(t, "kb/tasks/a/t099.md", last, "rows are sorted bytewise")

	// A commit lands between the pages. Page 2 compares the SAME two trees.
	writeF(t, svc, "main", "kb/tasks/a/zz-late.md")

	p2 := changes(t, svc, "main", ChangesQuery{Since: since, Head: p1.Head, Prefix: "tasks/a", After: last, Limit: 100})
	require.Equal(t, []PathChange{{Path: "kb/tasks/a/t100.md", Change: ChangeAdded}}, p2.Changes)
	require.False(t, p2.HasMore)
	require.Equal(t, p1.Head, p2.Head)
}

// Test 4b: a since that is not behind head is refused, never answered.
func TestChangesUnder_SinceNotBehindHeadRefused(t *testing.T) {
	svc := newChangesService(t)
	c1 := writeF(t, svc, "main", "kb/tasks/a/t1.md")
	c2 := writeF(t, svc, "main", "kb/tasks/a/t2.md")
	// Rewind main to c1: c2 now lies AHEAD of head, as a bookmark taken on a
	// machine whose main was further along would.
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), plumbing.NewHash(c1))))

	res, err := svc.Facts().ChangesUnder(context.Background(), "main", ChangesQuery{Since: c2, Prefix: "tasks/a"})
	require.ErrorIs(t, err, ErrSinceNotBehind)
	require.Empty(t, res.Changes, "a refusal carries no rows — above all, no fabricated deletion")

	// Sideways: a commit on another line of history is refused too.
	side := writeF(t, svc, "agent/a", "kb/tasks/a/side.md")
	_, err = svc.Facts().ChangesUnder(context.Background(), "main", ChangesQuery{Since: side, Prefix: "tasks/a"})
	require.ErrorIs(t, err, ErrSinceNotBehind)
}

// Test 4c: a folder missing at either end is the empty tree.
func TestChangesUnder_MissingFolderIsEmptyTree(t *testing.T) {
	svc := newChangesService(t)
	before := writeF(t, svc, "main", "kb/other/seed.md")

	// The lane's first post: tasks/new/ did not exist at since.
	post := writeF(t, svc, "main", "kb/tasks/new/first.md")
	res := changes(t, svc, "main", ChangesQuery{Since: before, Prefix: "tasks/new"})
	require.Equal(t, []PathChange{{Path: "kb/tasks/new/first.md", Change: ChangeAdded}}, res.Changes)

	// The lane's last task goes: git drops the folder at head.
	deleteF(t, svc, "main", "kb/tasks/new/first.md")
	res = changes(t, svc, "main", ChangesQuery{Since: post, Prefix: "tasks/new"})
	require.Equal(t, []PathChange{{Path: "kb/tasks/new/first.md", Change: ChangeDeleted}}, res.Changes)

	// Missing at both ends: empty, not an error.
	res = changes(t, svc, "main", ChangesQuery{Since: before, Prefix: "tasks/never"})
	require.Empty(t, res.Changes)
}

// Test 4d: prefix rules.
func TestNormalizeChangesPrefix(t *testing.T) {
	for _, in := range []string{"tasks/a", "tasks/a/", "/tasks/a", "Tasks/A", "/TASKS/a/"} {
		got, err := NormalizeChangesPrefix("kb", in)
		require.NoError(t, err, in)
		require.Equal(t, "kb/tasks/a", got, in)
	}
	got, err := NormalizeChangesPrefix("kb", "")
	require.NoError(t, err)
	require.Equal(t, "kb", got, "an empty prefix is the whole ontology root")

	got, err = NormalizeChangesPrefix("Facts", "x")
	require.NoError(t, err)
	require.Equal(t, "Facts/x", got, "the configured root is joined as-is, never a literal kb")

	got, err = NormalizeChangesPrefix("kb", "tasks/a.md")
	require.NoError(t, err)
	require.Equal(t, "kb/tasks/a.md", got, "no .md is added; a folder named like a file is taken literally")

	for _, bad := range []string{"../x", "tasks/../x", "tasks/.drafts", ".knomit/inbox", "tasks//a"} {
		_, err := NormalizeChangesPrefix("kb", bad)
		require.ErrorIs(t, err, ErrInvalidPrefix, bad)
	}
}

func TestChangesUnder_PrefixSpellingsAgree(t *testing.T) {
	svc := newChangesService(t)
	since := writeF(t, svc, "main", "kb/other/seed.md")
	writeF(t, svc, "main", "kb/tasks/a/t1.md")
	want := []PathChange{{Path: "kb/tasks/a/t1.md", Change: ChangeAdded}}
	for _, p := range []string{"tasks/a", "tasks/a/", "/tasks/a", "Tasks/A"} {
		require.Equal(t, want, changes(t, svc, "main", ChangesQuery{Since: since, Prefix: p}).Changes, p)
	}
}

// Test 5: siblings and private paths are excluded.
func TestChangesUnder_PrefixExcludesSiblingsAndPrivate(t *testing.T) {
	svc := newChangesService(t)
	since := writeF(t, svc, "main", "kb/other/seed.md")
	writeF(t, svc, "main", "kb/tasks/a/in.md")
	writeF(t, svc, "main", "kb/tasks/ab/sib.md")
	writeF(t, svc, "main", "kb/tasks/a-old/sib.md")
	writeF(t, svc, "main", "kb/tasks/a/.drafts/x.md")

	res := changes(t, svc, "main", ChangesQuery{Since: since, Prefix: "tasks/a"})
	require.Equal(t, []PathChange{{Path: "kb/tasks/a/in.md", Change: ChangeAdded}}, res.Changes)
}

// A rename is a deletion plus an addition: DiffTree does no rename detection.
func TestChangesUnder_RenameIsDeletedPlusAdded(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	since := writeF(t, svc, "main", "kb/tasks/a/old.md")
	body := testFactBody("kb/tasks/a/old.md", 0.8, nil) // identical bytes: a pure move
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{"kb/tasks/b/new.md": body}, []string{"kb/tasks/a/old.md"}, "move", "learn")
	require.NoError(t, err)

	res := changes(t, svc, "main", ChangesQuery{Since: since, Prefix: "tasks"})
	require.Equal(t, []PathChange{
		{Path: "kb/tasks/a/old.md", Change: ChangeDeleted},
		{Path: "kb/tasks/b/new.md", Change: ChangeAdded},
	}, res.Changes)
}

// Test 5b: the reviewer's P3d shape, driven through the real fast-forward. An
// agent commit x OLDER than the reader's bookmark m deletes a task; the agent
// merges m in as y; main fast-forwards to y. A time-ordered replay of the
// commit log after m returns only y — whose rows are its diff against x, so
// they show m's side — and never reports x's deletion. Two trees do.
func TestChangesUnder_DeletionSurvivesFastForwardOverOlderCommit(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	t0 := time.Unix(2_000_000, 0).UTC()

	writeF(t, svc, "agent/a", "kb/tasks/a/t0.md")
	writeF(t, svc, "agent/a", "kb/tasks/a/t1.md")
	c0 := restamp(t, svc, "agent/a", nil, t0.Add(1000*time.Second))
	_, err := svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)

	// Main moves on: m (the reader's bookmark) adds t2 at t=1010.
	writeF(t, svc, "main", "kb/tasks/a/t2.md")
	m := restamp(t, svc, "main", nil, t0.Add(1010*time.Second))

	// Agent, cut from c0, deletes t1 at t=1008 — BEFORE m in time.
	deleteF(t, svc, "agent/a", "kb/tasks/a/t1.md")
	x := restamp(t, svc, "agent/a", nil, t0.Add(1008*time.Second))

	// Agent merges m in: y has parents [x, m] and x's tree plus t2.
	writeF(t, svc, "agent/a", "kb/tasks/a/t2.md")
	y := restamp(t, svc, "agent/a", []plumbing.Hash{plumbing.NewHash(x), plumbing.NewHash(m)}, t0.Add(1011*time.Second))
	_ = c0

	res, err := svc.AdvanceLocalUpstream(ctx, "agent/a", "main")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode, "fixture: main must FAST-FORWARD to y, the path that hides x")
	tip, err := svc.rh.resolveRef(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, y, tip.String())

	got := changes(t, svc, "main", ChangesQuery{Since: m, Prefix: "tasks/a"})
	require.Equal(t, []PathChange{{Path: "kb/tasks/a/t1.md", Change: ChangeDeleted}}, got.Changes)
}

// Test 6: explicit failures, never an empty page.
func TestChangesUnder_ExplicitFailures(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	writeF(t, svc, "main", "kb/tasks/a/t1.md")

	for _, since := range []string{
		"0123456789abcdef0123456789abcdef01234567", // well-formed, not in this repo
		"abc123",                                   // short
		"not-a-hash-not-a-hash-not-a-hash-not-a-h", // 40 chars, not hex
	} {
		res, err := svc.Facts().ChangesUnder(ctx, "main", ChangesQuery{Since: since, Prefix: "tasks/a"})
		require.ErrorIs(t, err, ErrUnknownSince, since)
		require.Empty(t, res.Changes)
		require.Contains(t, err.Error(), "omit since")
	}

	_, err := svc.Facts().ChangesUnder(ctx, "no-such-branch", ChangesQuery{Prefix: "tasks/a"})
	require.ErrorIs(t, err, ErrBranchNotFound, "an unknown branch is an error, never a read across every branch")

	_, err = svc.Facts().ChangesUnder(ctx, "main", ChangesQuery{Head: "0123456789abcdef0123456789abcdef01234567", Prefix: "tasks/a"})
	require.ErrorIs(t, err, ErrInvalidChangesCursor)

	// A cursor head from another line of history is not this branch's.
	side := writeF(t, svc, "agent/a", "kb/tasks/a/side.md")
	_, err = svc.Facts().ChangesUnder(ctx, "main", ChangesQuery{Head: side, Prefix: "tasks/a"})
	require.ErrorIs(t, err, ErrInvalidChangesCursor)

	_, err = svc.Facts().ChangesUnder(ctx, "main", ChangesQuery{Prefix: "../x"})
	require.True(t, errors.Is(err, ErrInvalidPrefix))
}
