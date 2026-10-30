package vcs

import (
	"context"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// PathAtCommit is one (path, commit) pair of a batched tree read.
type PathAtCommit struct {
	Path, Commit string
}

// BlobHashesAtCommits is ReadBlobHashAtCommit for many pairs at once, with
// the same answer for each: the blob hash of the EXACT path in the commit's
// tree, present in the store as a blob; a pair that ReadBlobHashAtCommit
// would fail for (unknown commit, a missing path segment, a directory, a
// blob object not in the store) is absent from the map.
//
// It decodes each commit and each tree object at most ONCE per call (a
// subtree unchanged between commits has the same hash, so pairs at different
// commits share it too) and checks every blob's presence in one query per
// chunk, where ReadBlobHashAtCommit decodes the commit, every tree along the
// path and the blob object again for every pair. The decoded trees live only
// for this call: no object cache outlives it (see the storer's
// object-cache constraints, knomit#357).
func (rh *Repo) BlobHashesAtCommits(ctx context.Context, pairs []PathAtCommit) (map[PathAtCommit]string, error) {
	rh.gitTreeReads.Add(1)
	roots := map[string]*object.Tree{}
	trees := map[plumbing.Hash]*object.Tree{}
	found := make(map[PathAtCommit]plumbing.Hash, len(pairs))
	var blobs []plumbing.Hash
	seenBlob := map[plumbing.Hash]bool{}
	for _, p := range pairs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, done := found[p]; done {
			continue
		}
		root, ok := roots[p.Commit]
		if !ok {
			root = nil
			if commit, err := rh.repo.CommitObject(plumbing.NewHash(p.Commit)); err == nil {
				if t, err := commit.Tree(); err == nil {
					root = t
				}
			}
			roots[p.Commit] = root
		}
		if root == nil {
			continue
		}
		h, ok := rh.entryAt(root, p.Path, trees)
		if !ok {
			continue
		}
		found[p] = h
		if !seenBlob[h] {
			seenBlob[h] = true
			blobs = append(blobs, h)
		}
	}
	present, err := rh.gits.ObjectsOfType(ctx, plumbing.BlobObject, blobs)
	if err != nil {
		return nil, err
	}
	out := make(map[PathAtCommit]string, len(found))
	for p, h := range found {
		if present[h] {
			out[p] = h.String()
		}
	}
	return out, nil
}

// entryAt is the hash of path's entry under root, walking (and decoding,
// once per call) the trees along it, as object.Tree.FindEntry does: every
// segment but the last must name a tree; the last must not name a directory
// (Tree.File's blob read refuses a tree).
func (rh *Repo) entryAt(root *object.Tree, path string, trees map[plumbing.Hash]*object.Tree) (plumbing.Hash, bool) {
	parts := strings.Split(path, "/")
	t := root
	for _, dir := range parts[:len(parts)-1] {
		e, err := t.FindEntry(dir)
		if err != nil {
			return plumbing.ZeroHash, false
		}
		sub, ok := trees[e.Hash]
		if !ok {
			sub, err = object.GetTree(rh.repo.Storer, e.Hash)
			if err != nil {
				sub = nil
			}
			trees[e.Hash] = sub
		}
		if sub == nil {
			return plumbing.ZeroHash, false
		}
		t = sub
	}
	e, err := t.FindEntry(parts[len(parts)-1])
	if err != nil || e.Mode == filemode.Dir || e.Mode == filemode.Submodule {
		return plumbing.ZeroHash, false
	}
	return e.Hash, true
}
