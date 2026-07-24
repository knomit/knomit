package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
	"knomit/internal/okf"
)

const okfOntologyRoot = "kb"

// okfReadFacts enumerates every fact blob under kb/ in the tree at sourceSHA,
// parses it, and stamps each with its authoring time from the history walk.
// It reads the git tree directly (not the derived index) so the result is a
// pure function of the source commit.
func (s *Service) okfReadFacts(ctx context.Context, sourceSHA plumbing.Hash) ([]okf.FactInput, error) {
	_, authored, err := s.okfHistory(ctx, sourceSHA)
	if err != nil {
		return nil, err
	}

	commit, err := object.GetCommit(s.rh.gits, sourceSHA)
	if err != nil {
		return nil, fmt.Errorf("okf: get source commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("okf: source tree: %w", err)
	}

	var facts []okf.FactInput
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, okfOntologyRoot+"/") || !strings.HasSuffix(f.Name, ".md") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("okf: read %s: %w", f.Name, err)
		}
		parsed, err := fact.ParseFact(f.Name, content)
		if err != nil {
			// Non-fact markdown under kb/ (e.g. a stray README or the kb.md
			// manifest) is skipped: it is simply not a fact to export.
			return nil
		}
		ts := authored[f.Name]
		if ts.IsZero() {
			ts = commit.Committer.When // fallback: the exported commit's time
		}
		facts = append(facts, okf.FactInput{Fact: parsed, Timestamp: ts})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return facts, nil
}

// okfHistory walks commits from sourceSHA (bounded), producing log entries and
// a path→authoring-time map. Authoring time is the OLDEST commit that touched
// a path; an Update entry is emitted for each later commit that modified it.
// Deterministic per sourceSHA. Bounded to avoid unbounded walks on huge DAGs.
func (s *Service) okfHistory(ctx context.Context, sourceSHA plumbing.Hash) ([]okf.LogEntry, map[string]time.Time, error) {
	const maxCommits = 5000

	root, err := object.GetCommit(s.rh.gits, sourceSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("okf: get source commit: %w", err)
	}

	authored := map[string]time.Time{} // path -> earliest touch time
	var events []okf.LogEntry

	iter := object.NewCommitPreorderIter(root, nil, nil)
	seenCommits := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if seenCommits >= maxCommits {
			return object.ErrCanceled
		}
		seenCommits++

		changed, err := okfChangedFactPaths(s, c)
		if err != nil {
			return err
		}
		for _, ch := range changed {
			if prev, seen := authored[ch.path]; !seen || c.Committer.When.Before(prev) {
				authored[ch.path] = c.Committer.When
			}
			kind := "Update"
			if ch.created {
				kind = "Creation"
			}
			events = append(events, okf.LogEntry{
				Date:  c.Committer.When,
				Kind:  kind,
				Title: ch.title,
				Path:  ch.path,
			})
		}
		return nil
	})
	if err != nil && err != object.ErrCanceled {
		return nil, nil, err
	}

	// Normalize to exactly one Creation per path: the earliest Creation-marked
	// event. A path's Creation is decided by the diff (a file absent from the
	// parent tree), not by timestamp equality — commits sharing a wall-second
	// would otherwise both look "earliest" and both be labelled Creation. Any
	// remaining events (later touches, or a rare create/delete/recreate) are
	// Updates.
	creationAt := map[string]time.Time{} // path -> time of its Creation event
	for _, e := range events {
		if e.Kind != "Creation" {
			continue
		}
		if t, ok := creationAt[e.Path]; !ok || e.Date.Before(t) {
			creationAt[e.Path] = e.Date
		}
	}
	for i := range events {
		t, ok := creationAt[events[i].Path]
		if !(ok && events[i].Kind == "Creation" && events[i].Date.Equal(t)) {
			events[i].Kind = "Update"
		}
	}
	return events, authored, nil
}

type okfChange struct {
	path    string
	title   string
	created bool // the path was absent from the parent tree (an Insert)
}

// okfChangedFactPaths returns the kb/*.md paths added or modified by commit c
// relative to its first parent. For a root (parentless) commit, every kb/*.md
// file in its tree is treated as a creation.
func okfChangedFactPaths(s *Service, c *object.Commit) ([]okfChange, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}

	// Root commit: no parent to diff against. Enumerate the tree directly
	// rather than diffing against a storer-less empty Tree literal, which is
	// unreliable in go-git.
	if c.NumParents() == 0 {
		var out []okfChange
		err = tree.Files().ForEach(func(f *object.File) error {
			if ch, ok := okfChangeFromFile(f.Name, true, func() (string, error) { return f.Contents() }); ok {
				out = append(out, ch)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}

	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := parentTree.Diff(tree)
	if err != nil {
		return nil, err
	}

	var out []okfChange
	for _, ch := range changes {
		from, to, err := ch.Files()
		if err != nil {
			return nil, err
		}
		if to == nil {
			continue // deletion — not a creation/update event
		}
		// ch.To.Name is the full tree path (e.g. "kb/decisions/.../x.md");
		// to.Name from Files() is only the basename, so it cannot be used for
		// the ontology-prefix filter. from == nil means the path was absent
		// from the parent tree — an Insert, i.e. a Creation.
		if change, ok := okfChangeFromFile(ch.To.Name, from == nil, func() (string, error) { return to.Contents() }); ok {
			out = append(out, change)
		}
	}
	return out, nil
}

// okfChangeFromFile builds an okfChange for a kb/*.md path, reading the fact
// title best-effort. created reports whether the path was newly added by the
// commit. It returns ok=false for paths outside the ontology or non-.md files.
func okfChangeFromFile(name string, created bool, contents func() (string, error)) (okfChange, bool) {
	if !strings.HasPrefix(name, okfOntologyRoot+"/") || !strings.HasSuffix(name, ".md") {
		return okfChange{}, false
	}
	title := ""
	if content, err := contents(); err == nil {
		if f, err := fact.ParseFact(name, content); err == nil {
			title = f.Title
		}
	}
	return okfChange{path: name, title: title, created: created}, true
}
