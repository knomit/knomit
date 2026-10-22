package vcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"knomit/internal/domain/fact"

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
			return "", nil, fact.SymlinkNotFollowed(p)
		}
		s, err := f.Contents()
		return p, []byte(s), err
	}
	return "", nil, nil
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
