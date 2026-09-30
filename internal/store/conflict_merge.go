// Fact-level conflict merge: what a merge does with a path BOTH sides changed
// when the repo's `conflicts` attribute says merge, and the record every merge
// commit now carries of each conflict it settled (merged or side-picked).
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// The conflict record's trailer keys. They share the last paragraph of a merge
// (or replayed) commit's message with any other Knomit-* trailer.
const (
	// TrailerMerge names a path whose two versions were merged:
	//   Knomit-Merge: <path> strategy=<confidence|upstream> base=<blob|none>
	//     src=<blob|none> dst=<blob|none> out=<blob|none> decided=<field,…>
	// plus `dropped=<side>-modify` when one side deleted the fact and the
	// deletion won (out=none, decided=delete). Both input versions stay in
	// the commit's parents; the blobs name them.
	TrailerMerge = "Knomit-Merge"
	// TrailerConflict names a path whose conflict was settled by picking a
	// side, with the change that lost:
	//   Knomit-Conflict: <path> kept=<src|dst> dropped=<side>-<modify|delete|add>
	//     strategy=<conflict strategy> base=… src=… dst=… [reason=<why>]
	// reason is set when a merge was asked for and this path could not be
	// merged (not a fact, unparsable, kind mismatch, lossy), or when a human
	// chose the side for every path ("chosen").
	TrailerConflict = "Knomit-Conflict"
)

// mergeFactsRule is the fact.MergeRule a merge-facts strategy names; ok is
// false for a strategy that picks sides.
func mergeFactsRule(s ConflictStrategy) (fact.MergeRule, bool) {
	switch s {
	case StrategyMergeFacts:
		return fact.MergeConfidence, true
	case StrategyMergeFactsUpstream:
		return fact.MergeUpstream, true
	}
	return "", false
}

// mergeFactsStrategy is the ConflictStrategy for a `conflicts` rule.
func mergeFactsStrategy(rule fact.MergeRule) ConflictStrategy {
	if rule == fact.MergeUpstream {
		return StrategyMergeFactsUpstream
	}
	return StrategyMergeFacts
}

// factMerge is how one merge site runs a merge-facts strategy: which side is
// the consensus branch (for the upstream rule) and what a path that cannot be
// merged gets — the site's own side-picking strategy, unchanged.
type factMerge struct {
	rule     fact.MergeRule
	upstream fact.MergeSide
	fallback ConflictStrategy // StrategyLocalWins, StrategyRemoteWins or StrategyRefuse
}

// conflictShape is one conflicting path's three versions; a zero hash is an
// absent version.
type conflictShape struct {
	path           string
	base, src, dst plumbing.Hash
}

func shapeOf(path string, baseTree, srcTree, dstTree *object.Tree) conflictShape {
	c := conflictShape{path: path}
	c.base, _ = treeBlobHash(baseTree, path)
	c.src, _ = treeBlobHash(srcTree, path)
	c.dst, _ = treeBlobHash(dstTree, path)
	return c
}

// change names what one side did to the path relative to the base.
func (c conflictShape) change(side plumbing.Hash) string {
	switch {
	case c.base.IsZero():
		return "add"
	case side.IsZero():
		return "delete"
	default:
		return "modify"
	}
}

func blobName(h plumbing.Hash) string {
	if h.IsZero() {
		return "none"
	}
	return h.String()
}

func (c conflictShape) blobs() string {
	return "base=" + blobName(c.base) + " src=" + blobName(c.src) + " dst=" + blobName(c.dst)
}

// conflictLine is the Knomit-Conflict trailer for a side-pick: kept names the
// side that survived, and the other side's change is the one dropped.
func conflictLine(c conflictShape, kept ResolutionSide, strategy ConflictStrategy, reason string) string {
	dropped := "dst-" + c.change(c.dst)
	if kept == ResolveDst {
		dropped = "src-" + c.change(c.src)
	}
	line := fmt.Sprintf("%s: %s kept=%s dropped=%s strategy=%s %s", TrailerConflict, c.path, kept, dropped, strategy, c.blobs())
	if reason != "" {
		line += " reason=" + reason
	}
	return line
}

// factMergeResolutions settles every path the merge of src into dst conflicts
// on, for a merge-facts strategy, as the per-path resolutions the REFUSING
// walk applies (a Body can only set a path, so a deletion is a Side):
//
//   - src and dst hold the same bytes: dst, silently — nothing was lost.
//   - a fact path one side deleted and the other edited: the deletion wins
//     (ruling 2); the edit stays in the other parent.
//   - a fact path both sides changed or both added: fact.MergeVersions.
//   - anything else — not a fact path, or MergeVersions refused — gets the
//     site's own fallback strategy: LocalWins keeps dst (a dual-add takes src,
//     exactly as the LocalWins walk does), RemoteWins takes src, Refuse leaves
//     the path unresolved so the merge is refused as it always was.
//
// It returns the trailer lines for every path it settled.
func (rh *repoHandler) factMergeResolutions(
	ctx context.Context,
	baseCommit, srcCommit, dstCommit *object.Commit,
	fm factMerge,
) (map[string]Resolution, []string, error) {
	detected, err := rh.detectConflicts(ctx, baseCommit, srcCommit, dstCommit)
	if err != nil {
		return nil, nil, err
	}
	if len(detected) == 0 {
		return nil, nil, nil
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("base tree: %w", err)
	}
	srcTree, err := srcCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("src tree: %w", err)
	}
	dstTree, err := dstCommit.Tree()
	if err != nil {
		return nil, nil, fmt.Errorf("dst tree: %w", err)
	}
	paths := make([]string, 0, len(detected))
	for p := range detected {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	strategy := fact.MergeStrategy{Rule: fm.rule, Upstream: fm.upstream}
	res := make(map[string]Resolution, len(paths))
	var lines []string
	fallback := func(c conflictShape, reason string) {
		var kept ResolutionSide
		switch fm.fallback {
		case StrategyLocalWins:
			kept = ResolveDst
			if c.base.IsZero() {
				kept = ResolveSrc // the LocalWins walk overwrites a dual-add with src
			}
		case StrategyRemoteWins:
			kept = ResolveSrc
		default:
			return // Refuse: left unresolved, so the merge is refused
		}
		res[c.path] = Resolution{Side: kept}
		lines = append(lines, conflictLine(c, kept, fm.fallback, reason))
	}
	for _, p := range paths {
		c := shapeOf(p, baseTree, srcTree, dstTree)
		switch {
		case c.src == c.dst:
			res[p] = Resolution{Side: ResolveDst}
		case !rh.isFactPath(p):
			fallback(c, "not-a-fact")
		case c.src.IsZero() || c.dst.IsZero():
			// Modify/delete: the retraction wins. The walk's delete arm (src
			// deleted) applies ResolveSrc as a delete; its modify arm (dst
			// deleted) leaves the path absent under ResolveDst.
			kept, dropped := ResolveSrc, "dst-modify"
			if c.dst.IsZero() {
				kept, dropped = ResolveDst, "src-modify"
			}
			res[p] = Resolution{Side: kept}
			lines = append(lines, fmt.Sprintf("%s: %s strategy=%s %s out=none dropped=%s decided=delete",
				TrailerMerge, p, strategy.Name(), c.blobs(), dropped))
		default:
			var base []byte
			if !c.base.IsZero() {
				if base, err = rh.blobBytes(c.base); err != nil {
					return nil, nil, err
				}
			}
			src, err := rh.blobBytes(c.src)
			if err != nil {
				return nil, nil, err
			}
			dst, err := rh.blobBytes(c.dst)
			if err != nil {
				return nil, nil, err
			}
			out, rec, ok := fact.MergeVersions(p, base, src, dst, strategy)
			if !ok {
				fallback(c, rec.Reason)
				continue
			}
			res[p] = Resolution{Body: out}
			lines = append(lines, fmt.Sprintf("%s: %s strategy=%s %s out=%s decided=%s",
				TrailerMerge, p, rec.Strategy, c.blobs(), fact.GitBlobHash(out), strings.Join(rec.Decided, ",")))
		}
	}
	return res, lines, nil
}

// sideResolutionLines records a whole-set side choice (MergePushed's `side`)
// as one Knomit-Conflict line per conflicting path it settled.
func sideResolutionLines(baseCommit, srcCommit, dstCommit *object.Commit, detected map[string]bool, side ResolutionSide) ([]string, error) {
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, err
	}
	srcTree, err := srcCommit.Tree()
	if err != nil {
		return nil, err
	}
	dstTree, err := dstCommit.Tree()
	if err != nil {
		return nil, err
	}
	var lines []string
	for p := range detected {
		c := shapeOf(p, baseTree, srcTree, dstTree)
		if c.src == c.dst {
			continue
		}
		lines = append(lines, conflictLine(c, side, StrategyRefuse, "chosen"))
	}
	return lines, nil
}

func (rh *repoHandler) blobBytes(h plumbing.Hash) ([]byte, error) {
	blob, err := object.GetBlob(rh.gits, h)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", h, err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", h, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// conflictsLogged is the one-warning-per-(ontology blob) latch for a
// `conflicts` value this build cannot use.
var conflictsLogged sync.Map

// conflictsStrategy is the merge-facts strategy the ontology at branch's tip
// names, or ok=false when conflicts are not merged there: absent, off, a value
// this build does not know, no ontology, no such branch. The repo's owner
// decides it where every instance reads it — the tip of the CONSENSUS branch —
// so the caller passes that branch, never a hardcoded name.
func (rh *repoHandler) conflictsStrategy(ctx context.Context, branch string) (ConflictStrategy, bool) {
	h, err := rh.resolveRef(ctx, branch)
	if err != nil {
		if !errors.Is(err, ErrBranchNotFound) {
			log.Warn().Err(err).Str("branch", branch).Msg("conflicts: cannot resolve the consensus branch; read as off")
		}
		return "", false
	}
	c, err := object.GetCommit(rh.gits, h)
	if err != nil {
		log.Warn().Err(err).Str("branch", branch).Msg("conflicts: cannot read the consensus tip; read as off")
		return "", false
	}
	data, err := treeOntology(c)
	if err != nil || data == nil {
		return "", false
	}
	cs, err := fact.ReadConflicts(data)
	if err != nil || !cs.Valid {
		key := fact.GitBlobHash(data)
		if _, seen := conflictsLogged.LoadOrStore(key, true); !seen {
			log.Warn().Err(err).Interface("value", cs.Raw).Str("branch", branch).
				Msg(`conflicts: the ontology's value is unreadable or unknown (this knomit knows "off", "merge" and "merge:upstream"); read as off`)
		}
		return "", false
	}
	rule, on := cs.Rule()
	if !on {
		return "", false
	}
	return mergeFactsStrategy(rule), true
}

// appendTrailerLines adds lines to message's trailer paragraph: the last
// paragraph when it already is one (every line `Key: value`), so a replayed
// commit's Knomit-Trace stays readable, else a new last paragraph. Nil lines
// return message unchanged.
func appendTrailerLines(message string, lines []string) string {
	if len(lines) == 0 {
		return message
	}
	sorted := append([]string(nil), lines...)
	sort.Slice(sorted, func(i, j int) bool { return trailerSortKey(sorted[i]) < trailerSortKey(sorted[j]) })
	msg := strings.TrimRight(message, "\n")
	last := msg
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		last = msg[i+2:]
	}
	sep := "\n\n"
	if last != msg && isTrailerParagraph(last) {
		sep = "\n"
	}
	return msg + sep + strings.Join(sorted, "\n") + "\n"
}

// trailerSortKey orders the record by path, then key: "path\x00key".
func trailerSortKey(line string) string {
	key, rest, _ := strings.Cut(line, ": ")
	path, _, _ := strings.Cut(rest, " ")
	return path + "\x00" + key
}

func isTrailerParagraph(p string) bool {
	for _, line := range strings.Split(p, "\n") {
		k, _, ok := strings.Cut(line, ": ")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return false
		}
	}
	return true
}

// TrailerValues returns every value of the `Key: value` trailer key in the
// last paragraph of message, in order — the conflict record has one line per
// path.
func TrailerValues(message, key string) []string {
	msg := strings.TrimRight(message, "\n")
	last := msg
	if i := strings.LastIndex(msg, "\n\n"); i >= 0 {
		last = msg[i+2:]
	}
	var out []string
	for _, line := range strings.Split(last, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}
