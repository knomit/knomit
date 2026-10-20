package store

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/vcs"
)

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
		h, err := vcs.WriteBlobToStore(s, []byte(content))
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
