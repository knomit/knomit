package store

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// RuleValidation: a fact a branch's NET change ADDS OR CHANGES (branchFactsValid
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

// isFactPathUnder is the fact index's location rule (repoHandler.isFactPath)
// for an explicit root: a .md file under root with no dot-prefixed segment.
// Artifacts live beside the root, never under it, so they are never facts.
func isFactPathUnder(root, p string) bool {
	if root == "" {
		root = defaultOntologyRoot
	}
	return strings.HasPrefix(p, root+"/") && fact.IsMarkdownPath(p) && !fact.IsPrivatePath(p)
}

// newFactPaths lists, sorted, the fact paths (isFactPathUnder) whose content
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
			if n := ch.To.Name; n != "" && isFactPathUnder(root, n) {
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

// validateNewFacts runs the ontology's validations on every fact newFactPaths
// lists, as the MCP write path runs them: the topic path is the fact's
// directory under root (knomit_update derives it the same way), and the fact
// is the file as parsed. A file there that does not parse as a fact is no
// fact (the index skips it too) and is not judged. Returns the refusal
// reason, or "" when every new fact keeps every rule. A nil ontology has no
// rules; the context gate still runs (a fact with context and no ontology is
// refused, as on learn).
func validateNewFacts(ont *fact.Ontology, root string, froms []*object.Tree, to *object.Tree) (string, error) {
	if root == "" {
		root = defaultOntologyRoot
	}
	paths, err := newFactPaths(root, froms, to)
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		file, err := to.File(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		content, err := file.Contents()
		if err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
		f, err := fact.ParseFact(p, content)
		if err != nil {
			continue
		}
		topicPath := path.Dir(strings.TrimPrefix(p, root+"/"))
		if err := fact.ValidateFact(ont, topicPath, f); err != nil {
			var ve *fact.ValidationError
			if errors.As(err, &ve) {
				return fmt.Sprintf("%s fails the ontology's validation %q at %s: %s", p, ve.RuleName, ve.Topic, ve.Message), nil
			}
			return fmt.Sprintf("%s fails the ontology's validations: %v", p, err), nil
		}
	}
	return "", nil
}

// branchFactsValid is RuleValidation on a branch's NET change (#533 review
// B1, decided by the coordinator): every fact whose content at the branch's
// tip differs from main AND from the branch's merge base with main (so: the
// branch changed it, and main does not already hold that content) keeps every
// validation of ont, the judged repository's ontology read where the judge
// reads it (main, never the branch's own tree, so a pushed ontology cannot
// choose its own rules). A fact the branch did not touch is never judged,
// even when main has moved since the fork; a fact an earlier commit broke and
// a later one fixed is judged as fixed, so a branch is never stranded by a
// commit it has since corrected (an ontology main tightened after the
// commit was written included). Every OTHER rule of the judge stays per
// commit. main nil (no consensus branch yet) judges every fact at the tip.
// The refusal names the tip.
func branchFactsValid(tip, main *object.Commit, ont *fact.Ontology, root string) (Refusal, bool) {
	refuse := func(reason string) (Refusal, bool) {
		return Refusal{Commit: tip.Hash.String(), Rule: RuleValidation, Reason: reason}, false
	}
	tree, err := tip.Tree()
	if err != nil {
		return refuse(fmt.Sprintf("the branch's tree cannot be read: %v", err))
	}
	froms := []*object.Tree{nil}
	if main != nil {
		mt, err := main.Tree()
		if err != nil {
			return refuse(fmt.Sprintf("main's tree cannot be read: %v", err))
		}
		froms = []*object.Tree{mt}
		bases, err := tip.MergeBase(main)
		if err != nil {
			return refuse(fmt.Sprintf("the branch's merge base with main cannot be read: %v", err))
		}
		if len(bases) > 0 {
			bt, err := bases[0].Tree()
			if err != nil {
				return refuse(fmt.Sprintf("the merge base's tree cannot be read: %v", err))
			}
			froms = append(froms, bt)
		}
	}
	reason, err := validateNewFacts(ont, root, froms, tree)
	if err != nil {
		return refuse(fmt.Sprintf("the branch's facts cannot be read: %v", err))
	}
	if reason != "" {
		return refuse(reason)
	}
	return Refusal{}, true
}

// ontologyOf parses the ontology in c's tree: (nil, nil) when it holds none.
func ontologyOf(c *object.Commit) (*fact.Ontology, error) {
	raw, err := treeOntology(c)
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
