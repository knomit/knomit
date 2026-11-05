package vcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"knomit/internal/domain/fact"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// OntCache keeps the last ontology parsed per repository, keyed by the
// SHA-256 of its bytes: every write reads the ontology at the branch's tip,
// and parsing compiles its rules.
type OntCache struct {
	mu  sync.Mutex
	key [32]byte
	ont *fact.Ontology
	err error
	set bool
}

func (c *OntCache) Parse(raw []byte) (*fact.Ontology, error) {
	k := sha256.Sum256(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set && c.key == k {
		return c.ont, c.err
	}
	c.ont, c.err = fact.ParseOntology(raw)
	c.key, c.set = k, true
	return c.ont, c.err
}

// TreeOntology reads the ontology file from a commit's tree, trying every
// ontology path newest first. (nil, nil) when the tree has none. A symlink at
// the first rung present is fact.ErrSymlinkNotFollowed naming it: never its
// link text, never "none".
func TreeOntology(c *object.Commit) ([]byte, error) {
	_, data, err := TreeOntologyFile(c)
	return data, err
}

// TreeOntologyFile is TreeOntology also returning the rung it read ("" when
// none).
func TreeOntologyFile(c *object.Commit) (string, []byte, error) {
	p, f, err := treeOntologyEntry(c)
	if err != nil {
		return "", nil, err
	}
	if f == nil {
		return "", nil, nil
	}
	s, err := f.Contents()
	return p, []byte(s), err
}

// treeOntologyEntry is the rung walk every ontology reader shares: the first
// of fact.OntologyPathsNewestFirst present in c's tree, as a file whose bytes
// are not read yet ("", nil, nil when the tree has none). A symlink there is
// fact.ErrSymlinkNotFollowed naming it, returned WITH its path and entry so
// a caller that only wants to know whether the file changed can still name
// it. TreeOntologyFile and OntologyBlobAt both walk through here, so "which
// file is the ontology" has one answer whether a reader wants the bytes or
// only the blob id.
func treeOntologyEntry(c *object.Commit) (string, *object.File, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", nil, err
	}
	for _, p := range fact.OntologyPathsNewestFirst() {
		f, err := tree.File(p)
		if errors.Is(err, object.ErrFileNotFound) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if f.Mode == filemode.Symlink {
			return p, f, fact.SymlinkNotFollowed(p)
		}
		return p, f, nil
	}
	return "", nil, nil
}

// OntologyBlobAt resolves branch's tip and names the ontology file there by
// its blob id WITHOUT reading its bytes: the tip commit, the rung present
// ("" when none) and that entry's blob id (the zero hash when none). A
// symlink at the first rung present is fact.ErrSymlinkNotFollowed, returned
// with the tip, the path and the link's blob id. It is the cheap half of a
// repo's live ontology reload (repos.reloadOntology): an unchanged blob is
// an unchanged ontology, so nothing is read or parsed.
func (rh *Repo) OntologyBlobAt(ctx context.Context, branch string) (plumbing.Hash, string, plumbing.Hash, error) {
	h, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, "", plumbing.ZeroHash, err
	}
	c, err := object.GetCommit(rh.Storer(), h)
	if err != nil {
		return h, "", plumbing.ZeroHash, fmt.Errorf("OntologyBlobAt %s: %w", branch, err)
	}
	p, f, err := treeOntologyEntry(c)
	if f == nil {
		return h, p, plumbing.ZeroHash, err
	}
	return h, p, f.Hash, err
}

// OntologyFileAtCommit is TreeOntologyFile at commit: the rung read and its
// bytes ("", nil, nil when that tree has none).
func (rh *Repo) OntologyFileAtCommit(ctx context.Context, commit plumbing.Hash) (string, []byte, error) {
	c, err := object.GetCommit(rh.Storer(), commit)
	if err != nil {
		return "", nil, fmt.Errorf("OntologyFileAtCommit %s: %w", commit, err)
	}
	return TreeOntologyFile(c)
}

// OntologyFileAt is the ontology file at branch's tip, with the rung it read
// ("" when the tip has none); see Service.OntologyAt.
func (rh *Repo) OntologyFileAt(ctx context.Context, branch string) (string, []byte, error) {
	h, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", nil, err
	}
	c, err := object.GetCommit(rh.Storer(), h)
	if err != nil {
		return "", nil, fmt.Errorf("OntologyAt %s: %w", branch, err)
	}
	return TreeOntologyFile(c)
}
