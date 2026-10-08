package store

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestingCommitFiles builds its trees in one pass (testingTree) instead of one
// buildTree per path. The commit must still carry the SAME root tree that the
// per-path build gives: untouched entries kept, files added in existing and in
// new directories, an existing file replaced. Sabotage: drop the parent's
// untouched entries in testingDir.write, or write a directory with the leaf
// mode; the root hashes then differ.
func TestTestingCommitFiles_TreeMatchesPerPathBuild(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	for _, p := range []string{"kb/tasks/a/keep.md", "kb/tasks/a/replace.md", "kb/other/keep.md", "README.md"} {
		_, err := svc.Facts().WriteFact(ctx, "main", p, testFactBody(p, 0.5, nil), "seed", "")
		require.NoError(t, err)
	}
	parent, err := svc.rh.resolveRef(ctx, "main")
	require.NoError(t, err)
	parentCommit, err := svc.rh.repo.CommitObject(parent)
	require.NoError(t, err)
	parentTree, err := parentCommit.Tree()
	require.NoError(t, err)

	files := map[string]string{"kb/tasks/a/replace.md": "replaced", "kb/new.md": "top-level"}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("kb/tasks/a/%02d.md", i)] = fmt.Sprintf("a%d", i)         // an existing directory
		files[fmt.Sprintf("kb/tasks/m/%d/%02d.md", i%3, i)] = fmt.Sprintf("m%d", i) // new nested directories
	}

	// The expected root: the per-path build, over the same parent tree.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	want, tree := parentTree.Hash, parentTree
	for _, p := range paths {
		blob, err := writeBlobToStore(svc.rh.gits, []byte(files[p]))
		require.NoError(t, err)
		want, err = buildTree(svc.rh.gits, tree, p, blob)
		require.NoError(t, err)
		tree, err = object.GetTree(svc.rh.gits, want)
		require.NoError(t, err)
	}

	h, err := svc.TestingCommitFiles("main", files, "batch")
	require.NoError(t, err)
	c, err := svc.rh.repo.CommitObject(plumbing.NewHash(h))
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{parent}, c.ParentHashes)
	require.NotEqual(t, parentTree.Hash, c.TreeHash, "reached: the batch changed the tree")
	require.Equal(t, want, c.TreeHash, "the one-pass tree is the per-path tree")

	// Spot check through the commit: a kept file, a replaced one, a new one.
	got, err := c.Tree()
	require.NoError(t, err)
	for p, body := range map[string]string{"kb/tasks/a/replace.md": "replaced", "kb/tasks/m/2/29.md": "m29", "kb/new.md": "top-level"} {
		f, err := got.File(p)
		require.NoError(t, err, p)
		s, err := f.Contents()
		require.NoError(t, err)
		require.Equal(t, body, s, p)
	}
	_, err = got.File("kb/other/keep.md")
	require.NoError(t, err, "an untouched entry is kept")

	// A path that is both a file and a directory in one batch is refused.
	_, err = svc.TestingCommitFiles("main", map[string]string{"kb/x.md": "f", "kb/x.md/y.md": "d"}, "clash")
	require.ErrorContains(t, err, "both a file and a directory")
}
