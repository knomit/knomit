package facts

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/verify"
)

// ErrFactInvalid matches *FactInvalidError with errors.Is.
var ErrFactInvalid = errors.New("the fact fails the ontology's validations")

// FactInvalidError is a write refused at the source (writeFileExact,
// batchWriteLocked) because a fact it would add or change fails the
// validations of the ontology at the branch's tip (RuleValidation). Nothing
// was written. Err is the *fact.ValidationError (or the context gate's
// error), whose message is the ontology's own.
type FactInvalidError struct {
	Path string
	Err  error
}

func (e *FactInvalidError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }
func (e *FactInvalidError) Unwrap() error { return e.Err }
func (e *FactInvalidError) Is(target error) bool {
	return target == ErrFactInvalid
}

// ErrOntologyUnusable matches *OntologyUnusableError with errors.Is.
var ErrOntologyUnusable = errors.New("the repository's ontology is unusable; no fact can be written until it is fixed")

// OntologyUnusableError is a fact write refused because the repository's
// ontology cannot be used (missing, empty, unparseable, a symlink): no rule
// could judge the fact. Nothing was written. Err says why.
type OntologyUnusableError struct {
	Err error
}

func (e *OntologyUnusableError) Error() string {
	return fmt.Sprintf("%v: %v", ErrOntologyUnusable, e.Err)
}
func (e *OntologyUnusableError) Unwrap() error { return e.Err }
func (e *OntologyUnusableError) Is(target error) bool {
	return target == ErrOntologyUnusable
}

type carriedOverKey struct{}

// WithCarriedOver marks ctx's writes as a BYTE-IDENTICAL copy of content that
// already exists (#533 review B2): Replay, which rewrites every fact of a
// repository into the clone of a remote with disjoint history when the
// repository is connected to it, and any other bulk re-write of carried-over
// content. Those writes skip validateWrite, as the rule that an untouched
// fact is never judged requires: a legacy fact that fails a rule added after
// it was written must not abort a connect. Content a copy ALTERS (Replay's
// dead-ref rewriting, resolveDeadRefs) is never marked, and is judged like
// any write (#533 re-review N2); nor is a write that changes what a fact
// says.
func WithCarriedOver(ctx context.Context) context.Context {
	return context.WithValue(ctx, carriedOverKey{}, true)
}

func carriedOver(ctx context.Context) bool {
	v, _ := ctx.Value(carriedOverKey{}).(bool)
	return v
}

// validateWrite is RuleValidation at the source, for EVERY in-process writer
// (user ruling 2026-10-16: validations are honoured everywhere): REST fact
// writes, the review pipeline (distill, prune, reflect, dedup, discovery),
// the fleet code, trigger scripts and the MCP tools all commit through
// writeFileExact or batchWriteLocked, which call it under the branch lock
// before any object is written. Each file in files that is a fact
// (verify.IsFactPathUnder the fact root, not in deletes) and whose content differs
// from the blob at head is parsed and validated against the ontology at head,
// on its topic path (as knomit_update derives it). So an edit of a legacy
// fact that fails a rule must bring it into compliance, while a fact the
// write does not change is never judged. A branch with no ontology at its
// tip (a repository being created), or one whose ontology does not parse,
// runs nothing. A file that does not parse as a fact is left to the caller,
// as before.
func (rh *Repo) validateWrite(head plumbing.Hash, files map[string]string, deletes []string) error {
	if head.IsZero() || len(files) == 0 {
		return nil
	}
	root := rh.vr.OntologyRoot()
	var paths []string
	deleted := map[string]bool{}
	for _, d := range deletes {
		deleted[d] = true
	}
	for p := range files {
		if verify.IsFactPathUnder(root, p) && !deleted[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// The repository's ontology is unusable (#429 decision 1: missing, empty,
	// unparseable or a symlink at the read branch's tip, as the repo judged
	// it at Identify or on a live reload): NO fact is written, by any writer
	// — the review pipeline, a trigger, the fleet code — not only the
	// authored ones WritableBranch already refuses. A write here would be a
	// fact no ontology validated. A write that touches no fact (the
	// ontology's own repair, a dot path) never reaches this line.
	if uerr := rh.vr.OntologyUnusable(); uerr != nil {
		return &OntologyUnusableError{Err: uerr}
	}
	c, err := object.GetCommit(rh.vr.Storer(), head)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	raw, err := vcs.TreeOntology(c)
	if err != nil {
		return fmt.Errorf("validate: the ontology at %s: %w", head, err)
	}
	if raw == nil {
		return nil
	}
	ont, err := rh.vr.OntologyCache().Parse(raw)
	if err != nil {
		// No rule can be read, so no fact can be judged: the write is
		// REFUSED, never let through with its validations skipped (#429
		// decision 1; this used to return nil). A repair of the ontology
		// writes no fact and returned above, so it is not stopped. An
		// empty file is here too: it parses to an error.
		return &OntologyUnusableError{Err: fmt.Errorf("the ontology at %s does not parse: %w", head, err)}
	}
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		content := files[p]
		if f, err := tree.File(p); err == nil {
			if old, err := f.Contents(); err == nil && old == content {
				continue // unchanged: not this write's
			}
		}
		parsed, err := fact.ParseFact(p, content)
		if err != nil {
			continue
		}
		if err := fact.ValidateFact(ont, path.Dir(strings.TrimPrefix(p, root+"/")), parsed); err != nil {
			return &FactInvalidError{Path: p, Err: err}
		}
	}
	return nil
}
