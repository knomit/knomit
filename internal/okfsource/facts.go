package okfsource

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"knomit/internal/fact"
	"knomit/internal/okf"
)

// okfReadFacts enumerates every fact blob under kb/ in the tree at sourceSHA,
// parses it, and stamps each with its authoring time and revision list from
// hist. It reads the git tree directly (not any derived index) so the result is
// a pure function of the source commit.
//
// hist is passed in rather than computed here: the DAG walk is the expensive
// part of generation (a ParseFact + digest per changed path per commit), and
// the caller needs hist.Events anyway. Callers do the single walk and share it.
func okfReadFacts(st storer.EncodedObjectStorer, sourceSHA plumbing.Hash, hist okfHistoryResult, p Progress) ([]okf.FactInput, error) {
	commit, err := object.GetCommit(st, sourceSHA)
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
		ts := hist.Authored[f.Name]
		if ts.IsZero() {
			ts = commit.Committer.When // fallback: the exported commit's time
		}
		facts = append(facts, okf.FactInput{
			Fact:      parsed,
			Timestamp: ts,
			Revisions: hist.Revisions[f.Name],
		})
		if p != nil {
			p("facts", len(facts))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return facts, nil
}
