// Package source reads a knomit knowledge base from a git object store and
// produces the inputs internal/okf needs to render an OKF bundle.
//
// It is deliberately separate from internal/store: nothing here touches SQL,
// only go-git objects, so the exporter can run against a plain `git clone` of a
// knowledge base with no knomit server involved.
package source

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"knomit/internal/okf"
)

// Snapshot is everything the OKF mapper needs for one branch at one commit.
// It is a pure function of SourceSHA (plus the compiled-in default ontology),
// which is what lets a re-export of an unchanged source produce byte-identical
// output.
type Snapshot struct {
	SourceSHA plumbing.Hash
	// RepoID is the 12-hex prefix of the root commit — a knomit repo's stable
	// identity, identical in every clone and unaffected by renames.
	RepoID   string
	Facts    []okf.FactInput
	Events   []okf.LogEntry
	Retired  []okf.Retirement
	Ontology okf.OntologyDoc
	// Warnings records what was degraded rather than failed (an unparseable
	// ontology, say). The package does not log — callers decide how to surface
	// these, because a CLI and a server want different things.
	Warnings []string
}

// Progress reports incremental work while a Snapshot is being read. stage is
// "commits" or "facts"; done is the running count. It is called frequently and
// must not block — a caller that renders to a terminal should throttle.
type Progress func(stage string, done int)

// Load reads everything the OKF mapper needs for one branch at one commit.
//
// It works against ANY go-git storer — knomit's SQLite-backed store, a
// filesystem clone, or an in-memory fixture — because it only reads git
// objects. That portability is the whole point: the exporter runs against a
// plain `git clone` of a knowledge base, with no knomit server involved.
func Load(st storer.Storer, head plumbing.Hash) (Snapshot, error) {
	return LoadWithProgress(st, head, nil)
}

// LoadWithProgress is Load with a progress callback. The commit walk dominates
// the cost on a large knowledge base — seconds to minutes — so a caller with a
// user watching needs a way to show that it is advancing. A nil Progress is
// the silent path and costs nothing.
func LoadWithProgress(st storer.Storer, head plumbing.Hash, p Progress) (Snapshot, error) {
	hist, err := okfHistory(st, head, p)
	if err != nil {
		return Snapshot{}, err
	}
	facts, factWarns, err := okfReadFacts(st, head, hist, p)
	if err != nil {
		return Snapshot{}, err
	}
	root, err := rootCommit(st, head)
	if err != nil {
		return Snapshot{}, err
	}
	id := root.String()
	if len(id) > 12 {
		id = id[:12]
	}
	ont, warns := okfOntologyDoc(st, head)
	return Snapshot{
		SourceSHA: head,
		RepoID:    id,
		Facts:     facts,
		Events:    hist.Events,
		Retired:   hist.Retired,
		Ontology:  ont,
		// Order is the order the degradations happen in: the bounded walk
		// first, then what reading the tree found, then the ontology.
		Warnings: concat(hist.Warnings, factWarns, warns),
	}, nil
}

// concat joins warning lists, returning nil when every one of them is empty so
// a clean load reports no warnings rather than an empty non-nil slice.
func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// rootCommit walks first parents to the initial commit, whose hash is a knomit
// repo's stable identity — identical in every clone and unaffected by renames.
func rootCommit(st storer.EncodedObjectStorer, head plumbing.Hash) (plumbing.Hash, error) {
	c, err := object.GetCommit(st, head)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	for c.NumParents() > 0 {
		p, err := c.Parent(0)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		c = p
	}
	return c.Hash, nil
}
