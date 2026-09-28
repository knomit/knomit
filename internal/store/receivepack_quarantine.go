package store

import (
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// quarantine is where a pushed pack is unpacked before anything is decided
// about it. Writes go to memory; reads fall through to the host store, so a
// thin pack's REF_DELTA against an object the host already holds resolves
// (packfile.Parser looks external bases up in the storage it is given).
//
// Nothing reaches the host until promote: a refused push is dropped with the
// quarantine, which is how "objects from a refused push are not retained"
// holds (F11, R5) — the host's own storer writes each object immediately and
// could not be rolled back.
type quarantine struct {
	tmp  *memory.Storage
	host storer.EncodedObjectStorer
}

var _ storer.EncodedObjectStorer = (*quarantine)(nil)

func newQuarantine(host storer.EncodedObjectStorer) *quarantine {
	return &quarantine{tmp: memory.NewStorage(), host: host}
}

func (q *quarantine) NewEncodedObject() plumbing.EncodedObject { return &plumbing.MemoryObject{} }

func (q *quarantine) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	return q.tmp.SetEncodedObject(o)
}

func (q *quarantine) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	if o, err := q.tmp.EncodedObject(t, h); err == nil {
		return o, nil
	}
	return q.host.EncodedObject(t, h)
}

func (q *quarantine) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return q.tmp.IterEncodedObjects(t)
}

func (q *quarantine) HasEncodedObject(h plumbing.Hash) error {
	if q.held(h) {
		return nil
	}
	return q.host.HasEncodedObject(h)
}

func (q *quarantine) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	if s, err := q.tmp.EncodedObjectSize(h); err == nil {
		return s, nil
	}
	return q.host.EncodedObjectSize(h)
}

func (q *quarantine) AddAlternate(string) error {
	return errors.New("quarantine: alternates are not supported")
}

// held reports whether h arrived in this push (is in quarantine).
func (q *quarantine) held(h plumbing.Hash) bool {
	_, ok := q.tmp.ObjectStorage.Objects[h]
	return ok
}

// newCommits returns the commits reachable from tip that arrived in this push,
// tip first. The walk STOPS at any commit the host already holds rather than
// walking every host ref's history for a stop set (F11 review N9): the result
// is reachable(tip) ∖ reachable(host refs) in time proportional to the push,
// not to the repository. The two definitions differ only for commits the host
// holds that no ref reaches, and those are already present, so not judging
// them again is right either way.
//
// visited counts the commit objects decoded; it equals len(commits) exactly
// when the walk never descended into the host's history, which a test pins.
func (q *quarantine) newCommits(tip plumbing.Hash) (commits []*object.Commit, visited int, err error) {
	seen := map[plumbing.Hash]bool{}
	stack := []plumbing.Hash{tip}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] || !q.held(h) {
			continue
		}
		seen[h] = true
		c, err := object.GetCommit(q, h)
		visited++
		if err != nil {
			return nil, visited, fmt.Errorf("commit %s: %w", h, err)
		}
		commits = append(commits, c)
		for i := len(c.ParentHashes) - 1; i >= 0; i-- {
			stack = append(stack, c.ParentHashes[i])
		}
	}
	return commits, visited, nil
}

// reachableNew returns the quarantined objects reachable from commits (the
// result of newCommits): each commit and every tree and blob under it that
// arrived in this push. A pack may carry objects its ref does not reach;
// those are not promoted (review N2). A referenced object that is neither in
// quarantine nor on the host is an error: promoting the rest would leave the
// branch pointing at an incomplete tree.
func (q *quarantine) reachableNew(commits []*object.Commit) ([]plumbing.EncodedObject, error) {
	var out []plumbing.EncodedObject
	seen := map[plumbing.Hash]bool{}
	add := func(h plumbing.Hash) (bool, error) {
		if seen[h] {
			return false, nil
		}
		seen[h] = true
		if !q.held(h) {
			if err := q.host.HasEncodedObject(h); err != nil {
				return false, fmt.Errorf("object %s: neither sent nor held by this host", h)
			}
			return false, nil // the host has it, and therefore everything under it
		}
		o, err := q.tmp.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return false, err
		}
		out = append(out, o)
		return true, nil
	}
	var walkTree func(h plumbing.Hash) error
	walkTree = func(h plumbing.Hash) error {
		isNew, err := add(h)
		if err != nil || !isNew {
			return err
		}
		t, err := object.GetTree(q, h)
		if err != nil {
			return fmt.Errorf("tree %s: %w", h, err)
		}
		for _, e := range t.Entries {
			switch e.Mode {
			case filemode.Dir:
				if err := walkTree(e.Hash); err != nil {
					return err
				}
			case filemode.Submodule:
				// A gitlink names a commit in ANOTHER repository; there is no
				// object here to hold.
			default:
				if _, err := add(e.Hash); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, c := range commits {
		if _, err := add(c.Hash); err != nil {
			return nil, err
		}
		if err := walkTree(c.TreeHash); err != nil {
			return nil, err
		}
		for _, p := range c.ParentHashes {
			if _, err := add(p); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
