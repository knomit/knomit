package okfsource

import (
	"fmt"
	"path"
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
func okfReadFacts(st storer.EncodedObjectStorer, sourceSHA plumbing.Hash, hist okfHistoryResult, p Progress) ([]okf.FactInput, []string, error) {
	commit, err := object.GetCommit(st, sourceSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("okf: get source commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("okf: source tree: %w", err)
	}

	var facts []okf.FactInput
	var unparseable []string
	undated := 0
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
			//
			// A file that MEANT to be a fact is a different story — dropping it
			// silently deletes knowledge from a published base — so it is
			// reported. See looksLikeFact for how the two are told apart.
			if looksLikeFact(f.Name, content) {
				unparseable = append(unparseable, f.Name)
			}
			return nil
		}
		ts := hist.Authored[f.Name]
		if ts.IsZero() {
			// No commit in the walk touched this path. Absent a truncated walk
			// this cannot happen — every live fact was added by some visited
			// commit — so it is the fingerprint of the bound in okfHistory, and
			// the date below is today's, not the fact's.
			ts = commit.Committer.When
			undated++
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
		return nil, nil, err
	}

	var warnings []string
	if len(unparseable) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d file%s under %s/ could not be parsed as a fact and %s NOT exported: %s",
			len(unparseable), plural(len(unparseable)), okfOntologyRoot,
			was(len(unparseable)), summarize(unparseable, 5)))
	}
	if undated > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d fact%s carr%s the export commit's date rather than its own, because no commit in the walked history touched it",
			undated, plural(undated), map[bool]string{true: "ies", false: "y"}[undated == 1]))
	}
	return facts, warnings, nil
}

// factUUIDLen is the length of the uuid prefix fact.BuildFactPath names a fact
// file after (`kb/<topic>/<category>/<uuid8>.md`).
const factUUIDLen = 8

// looksLikeFact reports whether a kb/*.md file that FAILED to parse was
// nonetheless meant to be a fact, so its loss is worth reporting.
//
// Two independent signals, either of which is enough. A stray README or a
// hand-written note under kb/ has neither, so it stays silent as before:
//
//   - the basename is a fact uuid prefix, which is how every fact knomit writes
//     is named — that holds even for a file mangled beyond recognition;
//   - the content opens a frontmatter fence, i.e. it is shaped like a fact and
//     something inside it is wrong (bad YAML, a failed validation).
func looksLikeFact(name, content string) bool {
	if strings.HasPrefix(content, "---\n") {
		return true
	}
	base := strings.TrimSuffix(path.Base(name), ".md")
	if len(base) != factUUIDLen {
		return false
	}
	for _, r := range base {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// summarize lists at most max items, noting how many it left out — a corpus
// with hundreds of broken files must not bury the terminal in paths.
func summarize(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:max], ", "), len(items)-max)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func was(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}
