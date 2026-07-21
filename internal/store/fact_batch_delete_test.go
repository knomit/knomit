package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newBatchTestService opens a fresh initialised store for the batch tests.
func newBatchTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc
}

// TestBatchWriteFacts_WriteAndDeleteShareOneCommit is the regression test for
// the learn-subsume bug (P0.3): the retraction of a subsumed hypothesis used to
// be a separate DeleteFact commit, so a failure between the two could leave the
// new observation referencing a hypothesis it claimed to subsume. Writes and
// deletions passed to one BatchWriteFacts call must land in a SINGLE commit.
func TestBatchWriteFacts_WriteAndDeleteShareOneCommit(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	old, err := facts.WriteFact(ctx, "main", "kb/old.md", testFactBody("old", 0.5, nil), "seed", "")
	require.NoError(t, err)

	commit, blobs, err := facts.BatchWriteFacts(ctx, "main",
		map[string]string{"kb/new.md": testFactBody("new", 0.9, nil)},
		[]string{"kb/old.md"},
		"subsume", "learn")
	require.NoError(t, err)
	require.NotEmpty(t, commit)
	require.Contains(t, blobs, "kb/new.md")

	// The write is visible and the deletion applied — both at the same commit.
	_, err = facts.ReadFact(ctx, "main", "kb/new.md", nil)
	require.NoError(t, err, "batched write must be readable")

	_, err = facts.ReadFact(ctx, "main", "kb/old.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"batched delete must remove the fact, got %v", err)

	// One commit, not two: the batch commit's parent is the seed commit.
	added, modified, deleted, err := facts.DiffFiles(ctx, "main", old.CommitHash)
	require.NoError(t, err)
	require.Equal(t, []string{"kb/new.md"}, added)
	require.Empty(t, modified)
	require.Equal(t, []string{"kb/old.md"}, deleted)
}

// TestBatchWriteFacts_DeleteOnlyBatch covers the delete-only case: with no
// writes there is nothing to advance the running tree, so the commit must be
// seeded from the parent tree rather than committing an empty one.
func TestBatchWriteFacts_DeleteOnlyBatch(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	_, err := facts.WriteFact(ctx, "main", "kb/keep.md", testFactBody("keep", 0.5, nil), "seed", "")
	require.NoError(t, err)
	_, err = facts.WriteFact(ctx, "main", "kb/drop.md", testFactBody("drop", 0.5, nil), "seed", "")
	require.NoError(t, err)

	commit, _, err := facts.BatchWriteFacts(ctx, "main", nil, []string{"kb/drop.md"}, "retract", "learn")
	require.NoError(t, err)
	require.NotEmpty(t, commit)

	_, err = facts.ReadFact(ctx, "main", "kb/keep.md", nil)
	require.NoError(t, err, "untouched fact must survive a delete-only batch")
	_, err = facts.ReadFact(ctx, "main", "kb/drop.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound), "expected deletion, got %v", err)
}

// TestBatchWriteFacts_EmptyBatchIsNoOp: an all-empty call must not mint a
// commit. Callers (learn with nothing to retract) rely on this.
func TestBatchWriteFacts_EmptyBatchIsNoOp(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()

	commit, blobs, err := svc.Facts().BatchWriteFacts(ctx, "main", nil, nil, "nothing", "learn")
	require.NoError(t, err)
	require.Empty(t, commit)
	require.Empty(t, blobs)
}

// TestBatchWriteFacts_DeleteMissingPathFails: a deletion naming a path that
// isn't in the tree must fail the whole batch rather than silently committing
// the writes alone — the caller asked for both or neither.
func TestBatchWriteFacts_DeleteMissingPathFails(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	_, err := facts.WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.5, nil), "seed", "")
	require.NoError(t, err)

	_, _, err = facts.BatchWriteFacts(ctx, "main",
		map[string]string{"kb/b.md": testFactBody("b", 0.9, nil)},
		[]string{"kb/nested/missing.md"},
		"bad", "learn")
	require.Error(t, err)

	// The write was not committed on its own.
	_, err = facts.ReadFact(ctx, "main", "kb/b.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"a failed batch must not leave the write committed, got %v", err)
}
