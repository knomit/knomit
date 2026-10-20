package verify

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
	"knomit/internal/store/internal/vcs"
)

// RuleValidation: a fact a branch's NET change ADDS OR CHANGES (BranchFactsValid
// on the entry paths and the master's own publish; the merge result in the
// pushed-branch merges; the written content in the store's writers) fails one
// of the judged repository's ontology validations (fact.ValidateFact: the typed context
// gate, then the root, topic and declared-child rules). It is the same check
// the MCP write path (knomit_learn, knomit_update, experiment resolutions)
// makes before it writes, run where a commit ENTERS a branch others consume
// (user ruling 2026-10-16: "what's the point in supporting enforcements in
// the ontology definitions if they are not honored"). Unlike every other
// rule it does not ride verify_signatures: it is enforced in every mode,
// off included, in every repository, the fleet repository and every
// knowledge base alike, because the write path it mirrors refuses in every
// mode too.
const RuleValidation = "validation"

// RuleMergeConflict refuses, in every mode as RuleValidation does, a
// candidate whose merge into main does not come out clean (mergeResultValid):
// what a hand resolution would land on main cannot be judged.
const RuleMergeConflict = "merge-conflict"

// IsFactPathUnder is the fact index's location rule (repoHandler.IsFactPath)
// for an explicit root: a .md file under root with no dot-prefixed segment.
// Artifacts live beside the root, never under it, so they are never facts.
func IsFactPathUnder(root, p string) bool {
	if root == "" {
		root = vcs.DefaultOntologyRoot
	}
	return strings.HasPrefix(p, root+"/") && fact.IsMarkdownPath(p) && !fact.IsPrivatePath(p)
}

// newFactPaths lists, sorted, the fact paths (IsFactPathUnder) whose content
// in to differs from EVERY tree in froms (nil is the empty tree): the facts a
// commit with those parents ADDS OR CHANGES. A path that some parent already
// holds with the same blob is not new here — it was judged when that parent
// was, or it is history already on the branch (an untouched legacy fact never
// fails a push). That is the intersection of the per-parent changes, on
// purpose: a member's merge of main brings main's facts in against its own
// side, but adds none of them. Deletes are not listed: there is nothing left
// to validate.
func newFactPaths(root string, froms []*object.Tree, to *object.Tree) ([]string, error) {
	var keep map[string]bool
	for _, from := range froms {
		changes, err := object.DiffTree(from, to)
		if err != nil {
			return nil, fmt.Errorf("diff: %w", err)
		}
		here := map[string]bool{}
		for _, ch := range changes {
			if n := ch.To.Name; n != "" && IsFactPathUnder(root, n) {
				here[n] = true
			}
		}
		if keep == nil {
			keep = here
			continue
		}
		for p := range keep {
			if !here[p] {
				delete(keep, p)
			}
		}
	}
	out := make([]string, 0, len(keep))
	for p := range keep {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// ValidateNewFacts runs the ontology's validations on every fact newFactPaths
// lists, as the MCP write path runs them: the topic path is the fact's
// directory under root (knomit_update derives it the same way), and the fact
// is the file as parsed. A file there that does not parse as a fact is no
// fact (the index skips it too) and is not judged. Returns the refusal
// reason, or "" when every new fact keeps every rule. A nil ontology has no
// rules; the context gate still runs (a fact with context and no ontology is
// refused, as on learn).
func ValidateNewFacts(ont *fact.Ontology, root string, froms []*object.Tree, to *object.Tree) (string, error) {
	_, reason, err := firstInvalidFact(ont, root, froms, to)
	return reason, err
}

// firstInvalidFact is ValidateNewFacts also answering the failing fact's
// path ("" when none).
func firstInvalidFact(ont *fact.Ontology, root string, froms []*object.Tree, to *object.Tree) (string, string, error) {
	if root == "" {
		root = vcs.DefaultOntologyRoot
	}
	paths, err := newFactPaths(root, froms, to)
	if err != nil {
		return "", "", err
	}
	return invalidAmong(ont, root, paths, to)
}

// invalidAmong validates the facts at paths in to (as ValidateNewFacts does)
// and answers the first that fails: its path and the reason.
func invalidAmong(ont *fact.Ontology, root string, paths []string, to *object.Tree) (string, string, error) {
	for _, p := range paths {
		file, err := to.File(p)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", p, err)
		}
		content, err := file.Contents()
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", p, err)
		}
		f, err := fact.ParseFact(p, content)
		if err != nil {
			continue
		}
		topicPath := path.Dir(strings.TrimPrefix(p, root+"/"))
		if err := fact.ValidateFact(ont, topicPath, f); err != nil {
			var ve *fact.ValidationError
			if errors.As(err, &ve) {
				return p, fmt.Sprintf("%s fails the ontology's validation %q at %s: %s", p, ve.RuleName, ve.Topic, ve.Message), nil
			}
			return p, fmt.Sprintf("%s fails the ontology's validations: %v", p, err), nil
		}
	}
	return "", "", nil
}

// blobIn is the blob at p in t (false when t holds none there; t nil is the
// empty tree).
func blobIn(t *object.Tree, p string) (plumbing.Hash, bool) {
	if t == nil {
		return plumbing.ZeroHash, false
	}
	e, err := t.FindEntry(p)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return e.Hash, true
}

// introducedBy is the commit of tip ∖ main that wrote p's content at tip:
// a commit whose tree holds tip's blob at p while none of its parents does.
// A NON-MERGE such commit is preferred (#533 re-review S2: a merge cannot be
// dropped, and the content was written by a commit under it); a merge is
// named only when no non-merge commit wrote it (the merge itself resolved
// the path to new content). Among several (the same content written twice),
// the LATEST by topology: one that is not an ancestor of another candidate,
// the one closest to tip when several are unrelated (committer dates, which
// the author controls, are not read: re-review N1). The tip itself when none
// is found. main nil walks tip's whole history.
func introducedBy(tip, main *object.Commit, p string) plumbing.Hash {
	tipTree, err := tip.Tree()
	if err != nil {
		return tip.Hash
	}
	want, ok := blobIn(tipTree, p)
	if !ok {
		return tip.Hash
	}
	blobAt := func(c *object.Commit) (plumbing.Hash, bool) {
		t, err := c.Tree()
		if err != nil {
			return plumbing.ZeroHash, false
		}
		return blobIn(t, p)
	}
	// The commits' own storers (a push's quarantine reads through to the
	// host), walked by parent links, breadth first from tip.
	stop := map[plumbing.Hash]bool{}
	if main != nil {
		queue := []*object.Commit{main}
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			if stop[c.Hash] {
				continue
			}
			stop[c.Hash] = true
			for i := range c.NumParents() {
				if pc, err := c.Parent(i); err == nil {
					queue = append(queue, pc)
				}
			}
		}
	}
	var writers, mergeWriters []*object.Commit
	seen := map[plumbing.Hash]bool{}
	queue := []*object.Commit{tip}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c.Hash] || stop[c.Hash] {
			continue
		}
		seen[c.Hash] = true
		var parents []*object.Commit
		for i := range c.NumParents() {
			if pc, err := c.Parent(i); err == nil {
				parents = append(parents, pc)
			}
		}
		queue = append(queue, parents...)
		if h, ok := blobAt(c); !ok || h != want {
			continue
		}
		wrote := true
		for _, pc := range parents {
			if h, ok := blobAt(pc); ok && h == want {
				wrote = false
			}
		}
		if !wrote {
			continue
		}
		if c.NumParents() > 1 {
			mergeWriters = append(mergeWriters, c)
		} else {
			writers = append(writers, c)
		}
	}
	if len(writers) == 0 {
		writers = mergeWriters
	}
	// writers are in breadth-first order from tip; the first that is not an
	// ancestor of another writer is the latest.
	for _, c := range writers {
		latest := true
		for _, o := range writers {
			if o.Hash == c.Hash {
				continue
			}
			if anc, err := c.IsAncestor(o); err == nil && anc {
				latest = false
				break
			}
		}
		if latest {
			return c.Hash
		}
	}
	return tip.Hash
}

// BranchFactsValid is RuleValidation on a branch's NET change (#533 review
// B1, decided by the coordinator): every fact whose content at the branch's
// tip differs from main AND from its content at ONE OR MORE of the branch's
// merge bases with main (so: the branch changed it against some base, and
// main does not already hold that content) keeps every
// validation of ont, the judged repository's ontology read where the judge
// reads it (main, never the branch's own tree, so a pushed ontology cannot
// choose its own rules). A fact the branch did not touch is never judged,
// even when main has moved since the fork; a fact an earlier commit broke and
// a later one fixed is judged as fixed, so a branch is never stranded by a
// commit it has since corrected (an ontology main tightened after the
// commit was written included). Every OTHER rule of the judge stays per
// commit. main nil (no consensus branch yet) judges every fact at the tip.
// The refusal names the commit that wrote the failing content
// (introducedBy), the one a fix or a drop has to deal with, not the tip.
func BranchFactsValid(tip, main *object.Commit, ont *fact.Ontology, root string) (Refusal, bool) {
	refuse := func(reason string) (Refusal, bool) {
		return Refusal{Commit: tip.Hash.String(), Rule: RuleValidation, Reason: reason}, false
	}
	tree, err := tip.Tree()
	if err != nil {
		return refuse(fmt.Sprintf("the branch's tree cannot be read: %v", err))
	}
	// The facts that differ from main.
	var mainTree *object.Tree
	var baseTrees []*object.Tree
	if main != nil {
		if mainTree, err = main.Tree(); err != nil {
			return refuse(fmt.Sprintf("main's tree cannot be read: %v", err))
		}
		bases, err := tip.MergeBase(main)
		if err != nil {
			return refuse(fmt.Sprintf("the branch's merge bases with main cannot be read: %v", err))
		}
		for _, b := range bases {
			bt, err := b.Tree()
			if err != nil {
				return refuse(fmt.Sprintf("a merge base's tree cannot be read: %v", err))
			}
			baseTrees = append(baseTrees, bt)
		}
	}
	candidates, err := newFactPaths(root, []*object.Tree{mainTree}, tree)
	if err != nil {
		return refuse(fmt.Sprintf("the branch's facts cannot be read: %v", err))
	}
	// Skipped only when the content equals its content at EVERY merge base
	// (#533 re-review B1): with several best common ancestors (a criss-cross)
	// git's merge builds a virtual base from all of them, and a content equal
	// to just one base (go-git answers them newest first, a date the pusher
	// sets) can still be what the merge lands on main.
	var paths []string
	for _, p := range candidates {
		tb, _ := blobIn(tree, p)
		untouched := len(baseTrees) > 0
		for _, bt := range baseTrees {
			if bb, ok := blobIn(bt, p); !ok || bb != tb {
				untouched = false
				break
			}
		}
		if !untouched {
			paths = append(paths, p)
		}
	}
	if root == "" {
		root = vcs.DefaultOntologyRoot
	}
	p, reason, err := invalidAmong(ont, root, paths, tree)
	if err != nil {
		return refuse(fmt.Sprintf("the branch's facts cannot be read: %v", err))
	}
	if reason != "" {
		return Refusal{Commit: introducedBy(tip, main, p).String(), Rule: RuleValidation, Reason: reason}, false
	}
	return Refusal{}, true
}

// OntologyOf parses the ontology in c's tree: (nil, nil) when it holds none.
func OntologyOf(c *object.Commit) (*fact.Ontology, error) {
	raw, err := vcs.TreeOntology(c)
	if err != nil || raw == nil {
		return nil, err
	}
	return fact.ParseOntology(raw)
}

// ValidationRefusedError is a merge into the host's agent branch (MergePushed,
// MergeConsensus) refused because a fact the merge RESULT adds or changes
// against that branch fails the ontology's validations (RuleValidation).
// Nothing was written.
type ValidationRefusedError struct {
	Src, Dst, SrcTip string
	Reason           string
}

// ErrValidationRefused matches *ValidationRefusedError with errors.Is.
var ErrValidationRefused = errors.New("the merge would bring a fact that fails the ontology's validations")

func (e *ValidationRefusedError) Error() string {
	return fmt.Sprintf("%s: merging %s at %s into %s: %s", ErrValidationRefused.Error(), e.Src, e.SrcTip, e.Dst, e.Reason)
}

func (e *ValidationRefusedError) Is(target error) bool { return target == ErrValidationRefused }
