package store

import (
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
)

var errWantNotAdvertised = errors.New("want is not an advertised tip")

// packObjects is the ONE object-set builder for upload-pack. A request without
// a depth is an unlimited walk (what go-git's built-in server did); a request
// with "deepen N" is the same walk cut at N commits from each want. go-git's
// server has no shallow implementation at all and rejects the capability, so
// the wizard's depth-1 probe used to come back as an HTTP 500.
//
// Client shallows (the "shallow <sha>" lines in the request) play two
// different roles depending on whether the request also carries a depth:
//
//   - NO depth (an ordinary fetch by an already-shallow client): they are
//     GRAFT POINTS. The client does not hold their parents and is not asking
//     for them, so the walk stops there — without this a routine `git pull`
//     into a depth-1 clone drags the entire history across.
//   - WITH a depth: the depth cut governs, and the shallows are only consulted
//     to work out what changed. Grafting here would make deepening impossible.
//
// They always bound the HAVE walk: a shallow client holds nothing below its
// own boundary, so nothing there may be assumed present.
func packObjects(rh *repoHandler, req *packp.UploadPackRequest) ([]plumbing.Hash, packp.ShallowUpdate, error) {
	depth := 0
	if d, ok := req.Depth.(packp.DepthCommits); ok {
		depth = int(d)
	}
	clientShallow := map[plumbing.Hash]struct{}{}
	for _, h := range req.Shallows {
		clientShallow[h] = struct{}{}
	}

	wantStop := clientShallow
	if depth > 0 {
		wantStop = nil
	}

	// Commits the client wants, cut at depth.
	wantCommits, boundary, err := walkCommits(rh, req.Wants, depth, wantStop)
	if err != nil {
		return nil, packp.ShallowUpdate{}, err
	}

	// Commits the client already has, never descending below its shallows.
	haveCommits, _, err := walkCommits(rh, presentOnly(rh, req.Haves), 0, clientShallow)
	if err != nil {
		return nil, packp.ShallowUpdate{}, err
	}

	// Objects: every want-side commit plus its complete tree (a shallow clone
	// has a complete checkout at every commit it holds), minus everything
	// reachable from the have-side commits.
	haveObjs := map[plumbing.Hash]struct{}{}
	for h := range haveCommits {
		haveObjs[h] = struct{}{}
		if err := addTreeObjects(rh, h, haveObjs); err != nil {
			return nil, packp.ShallowUpdate{}, err
		}
	}
	seen := map[plumbing.Hash]struct{}{}
	var objs []plumbing.Hash
	add := func(h plumbing.Hash) {
		if _, dup := seen[h]; dup {
			return
		}
		if _, has := haveObjs[h]; has {
			return
		}
		seen[h] = struct{}{}
		objs = append(objs, h)
	}
	for h := range wantCommits {
		add(h)
		treeObjs := map[plumbing.Hash]struct{}{}
		if err := addTreeObjects(rh, h, treeObjs); err != nil {
			return nil, packp.ShallowUpdate{}, err
		}
		for t := range treeObjs {
			add(t)
		}
	}

	// Shallow update, relative to what the client already reports: git's own
	// upload-pack emits only CHANGES (send_shallow skips anything already
	// flagged CLIENT_SHALLOW), which is what makes the stateless rounds
	// idempotent — the client re-sends the same shallow lines every round.
	var upd packp.ShallowUpdate
	for h := range boundary {
		if _, already := clientShallow[h]; !already {
			upd.Shallows = append(upd.Shallows, h)
		}
	}
	for h := range clientShallow {
		if _, still := boundary[h]; still {
			continue
		}
		// Unshallow means "you now hold this commit's parents". The commit
		// being transferred is not enough — its PARENTS have to be in the
		// set, or the client would drop a graft point it still needs.
		if parentsTransferred(rh, h, wantCommits) {
			upd.Unshallows = append(upd.Unshallows, h)
		}
	}
	sortHashes(upd.Shallows)
	sortHashes(upd.Unshallows)
	return objs, upd, nil
}

// parentsTransferred reports whether every parent of commit h is in set.
// A commit absent from set (not walked at all) is not transferred, so it
// cannot be unshallowed.
func parentsTransferred(rh *repoHandler, h plumbing.Hash, set map[plumbing.Hash]struct{}) bool {
	if _, ok := set[h]; !ok {
		return false
	}
	c, err := rh.repo.CommitObject(h)
	if err != nil {
		return false
	}
	for _, p := range c.ParentHashes {
		if _, ok := set[p]; !ok {
			return false
		}
	}
	return true
}

func sortHashes(hs []plumbing.Hash) {
	sort.Slice(hs, func(i, j int) bool { return hs[i].String() < hs[j].String() })
}

// walkCommits walks parents breadth-first from starts. depth 0 means
// unlimited. stop commits are visited but not descended below. It returns the
// visited set and the boundary: commits reached at exactly depth that still
// have parents — the ones a shallow client records as its new graft points.
func walkCommits(rh *repoHandler, starts []plumbing.Hash, depth int, stop map[plumbing.Hash]struct{}) (visited, boundary map[plumbing.Hash]struct{}, err error) {
	visited = map[plumbing.Hash]struct{}{}
	boundary = map[plumbing.Hash]struct{}{}
	type item struct {
		h plumbing.Hash
		d int
	}
	queue := make([]item, 0, len(starts))
	for _, s := range starts {
		queue = append(queue, item{s, 1})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if _, done := visited[it.h]; done {
			continue
		}
		c, err := rh.repo.CommitObject(it.h)
		if err != nil {
			return nil, nil, fmt.Errorf("walk: commit %s: %w", it.h, err)
		}
		visited[it.h] = struct{}{}
		if _, isStop := stop[it.h]; isStop {
			continue
		}
		if depth > 0 && it.d >= depth {
			if c.NumParents() > 0 {
				boundary[it.h] = struct{}{}
			}
			continue
		}
		for _, p := range c.ParentHashes {
			queue = append(queue, item{p, it.d + 1})
		}
	}
	return visited, boundary, nil
}

// addTreeObjects adds the commit's tree, every subtree and every blob to out.
func addTreeObjects(rh *repoHandler, commit plumbing.Hash, out map[plumbing.Hash]struct{}) error {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return err
	}
	tree, err := c.Tree()
	if err != nil {
		return err
	}
	out[tree.Hash] = struct{}{}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		_, entry, err := w.Next()
		if err != nil {
			break // io.EOF
		}
		out[entry.Hash] = struct{}{}
	}
	return nil
}

// presentOnly keeps the haves this store actually holds; git clients may
// offer haves from other remotes.
func presentOnly(rh *repoHandler, haves []plumbing.Hash) []plumbing.Hash {
	var out []plumbing.Hash
	for _, h := range haves {
		if rh.gits.HasEncodedObject(h) == nil {
			out = append(out, h)
		}
	}
	return out
}
