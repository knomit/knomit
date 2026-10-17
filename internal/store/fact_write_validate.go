package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
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

// ontCache keeps the last ontology parsed per repository, keyed by the
// SHA-256 of its bytes: every write reads the ontology at the branch's tip,
// and parsing compiles its rules.
type ontCache struct {
	mu  sync.Mutex
	key [32]byte
	ont *fact.Ontology
	err error
	set bool
}

func (c *ontCache) parse(raw []byte) (*fact.Ontology, error) {
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

func (rh *repoHandler) ontologyCache() *ontCache { return &rh.writeOnt }

type carriedOverKey struct{}

// WithCarriedOver marks ctx's writes as a COPY of content that already
// exists (#533 review B2): Replay, which rewrites every fact of a repository
// into the clone of a remote with disjoint history when the repository is
// connected to it (its ref rewriting is mechanical), and any other bulk
// re-write of carried-over content. Those writes skip validateWrite, as the
// rule that an untouched fact is never judged requires: a legacy fact that
// fails a rule added after it was written must not abort a connect. A write
// that CHANGES what a fact says is never one of these.
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
// (isFactPathUnder the fact root, not in deletes) and whose content differs
// from the blob at head is parsed and validated against the ontology at head,
// on its topic path (as knomit_update derives it). So an edit of a legacy
// fact that fails a rule must bring it into compliance, while a fact the
// write does not change is never judged. A branch with no ontology at its
// tip (a repository being created), or one whose ontology does not parse,
// runs nothing. A file that does not parse as a fact is left to the caller,
// as before.
func (rh *repoHandler) validateWrite(head plumbing.Hash, files map[string]string, deletes []string) error {
	if head.IsZero() || len(files) == 0 {
		return nil
	}
	root := rh.ontologyRoot()
	var paths []string
	deleted := map[string]bool{}
	for _, d := range deletes {
		deleted[d] = true
	}
	for p := range files {
		if isFactPathUnder(root, p) && !deleted[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	c, err := object.GetCommit(rh.gits, head)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	raw, err := treeOntology(c)
	if err != nil {
		return fmt.Errorf("validate: the ontology at %s: %w", head, err)
	}
	if raw == nil {
		return nil
	}
	ont, err := rh.ontologyCache().parse(raw)
	if err != nil {
		// No rule can be read. Whether such a repository takes writes at all
		// is decided where it is opened (repos.loadOntology: unwritable), and
		// every entry path refuses a commit judged against it; refusing here
		// too would only stop a repair of the ontology's own branch.
		return nil
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
