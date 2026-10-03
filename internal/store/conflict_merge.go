// Conflict settlement: what a merge does with a path BOTH sides changed when
// the repo's `conflicts` attribute (an object: facts, state) says to settle it
// rather than pick a side, and the record every merge commit now carries of
// each conflict it settled (merged or side-picked).
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
	//   Knomit-Merge: <path> strategy=<merge|merge_consensus> base=<blob|none>
	//     src=<blob|none> dst=<blob|none> out=<blob|none> decided=<field,…>
	// plus `dropped=<side>-modify` when one side deleted the fact and the
	// deletion won (out=none, decided=delete). Both input versions stay in
	// the commit's parents; the blobs name them.
	TrailerMerge = "Knomit-Merge"
	// TrailerConflict names a path whose conflict was settled by picking a
	// side, with the change that lost:
	//   Knomit-Conflict: <path> kept=<src|dst> dropped=<side>-<modify|delete|add>
	//     strategy=<consensus|site strategy> base=… src=… dst=… [reason=<why>]
	// strategy=consensus names a pick of the consensus side (`facts:
	// consensus`, or `state: consensus`); any other strategy is the site's
	// own side-pick. reason is set when a merge was asked for and this path
	// could not be merged (not a fact, unparsable, kind mismatch, lossy), or
	// when a human chose the side for every path ("chosen").
	TrailerConflict = "Knomit-Conflict"
)

// recordConsensus is the strategy a Knomit-Conflict line names when the
// consensus side's version was taken. It is a record name only, never a
// strategy a merge runs.
const recordConsensus ConflictStrategy = "consensus"

// conflictsStrategyPrefix starts every ConflictStrategy that carries a
// `conflicts` policy: "conflicts:<facts>/<state>".
const conflictsStrategyPrefix = "conflicts:"

// conflictsPolicy is the `conflicts` setting as a merge site runs it: the two
// keys' values (fact.Conflicts*). It travels as a ConflictStrategy value, so
// every site's signature stays the one it had, and the merge commit's subject
// names it.
type conflictsPolicy struct {
	facts string // off | merge | merge:consensus | consensus
	state string // off | consensus
}

func (p conflictsPolicy) strategy() ConflictStrategy {
	return ConflictStrategy(conflictsStrategyPrefix + p.facts + "/" + p.state)
}

// conflictsPolicyOf is the policy a strategy carries; ok is false for a
// strategy that picks sides (or refuses) and for a malformed value.
func conflictsPolicyOf(s ConflictStrategy) (conflictsPolicy, bool) {
	rest, ok := strings.CutPrefix(string(s), conflictsStrategyPrefix)
	if !ok {
		return conflictsPolicy{}, false
	}
	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return conflictsPolicy{}, false
	}
	p := conflictsPolicy{facts: rest[:i], state: rest[i+1:]}
	switch p.facts {
	case fact.ConflictsOff, fact.ConflictsMerge, fact.ConflictsMergeConsensus, fact.ConflictsConsensus:
	default:
		return conflictsPolicy{}, false
	}
	switch p.state {
	case fact.ConflictsOff, fact.ConflictsConsensus:
	default:
		return conflictsPolicy{}, false
	}
	return p, true
}

// factMerge is how one merge site runs a `conflicts` policy: which side is the
// consensus side, and what a key set to off gets — the site's own
// side-picking strategy, unchanged.
type factMerge struct {
	policy    conflictsPolicy
	consensus fact.MergeSide   // the side that is on, or will reach, the consensus branch
	fallback  ConflictStrategy // StrategyLocalWins, StrategyRemoteWins or StrategyRefuse
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
// on, for a `conflicts` policy, as the per-path resolutions the REFUSING walk
// applies (a Body can only set a path, so a deletion is a Side):
//
//   - src and dst hold the same bytes: dst, silently — nothing was lost.
//   - not a fact path: the STATE rule.
//   - a fact path, `facts: off`: the site's own side-pick.
//   - a fact path, `facts: consensus`: the consensus side's whole version —
//     its edit, its deletion or its addition, unparsed.
//   - a fact path one side deleted and the other edited, `facts: merge` or
//     `merge:consensus`: the deletion wins (ruling 2); the edit stays in the
//     other parent.
//   - a fact path both sides changed or both added, `facts: merge` or
//     `merge:consensus`: fact.MergeVersions; what it refuses (unparsable,
//     kinds differ, lossy) gets the STATE rule, with MergeVersions' reason.
//
// The state rule: `state: consensus` takes the consensus side's version;
// `state: off` is the site's own side-pick. The site's side-pick is its
// fallback strategy: LocalWins keeps dst (a dual-add takes src, exactly as the
// LocalWins walk does), RemoteWins takes src, Refuse leaves the path
// unresolved so the merge is refused as it always was.
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

	consensusSide := ResolveDst
	if fm.consensus == fact.MergeSrc {
		consensusSide = ResolveSrc
	}
	rule, fieldMerge := fact.ConflictsFactsRule(fm.policy.facts)
	strategy := fact.MergeStrategy{Rule: rule, Consensus: fm.consensus}
	res := make(map[string]Resolution, len(paths))
	var lines []string
	sitePick := func(c conflictShape, reason string) {
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
	takeConsensus := func(c conflictShape, reason string) {
		res[c.path] = Resolution{Side: consensusSide}
		lines = append(lines, conflictLine(c, consensusSide, recordConsensus, reason))
	}
	stateRule := func(c conflictShape, reason string) {
		if fm.policy.state == fact.ConflictsConsensus {
			takeConsensus(c, reason)
			return
		}
		sitePick(c, reason)
	}
	for _, p := range paths {
		c := shapeOf(p, baseTree, srcTree, dstTree)
		switch {
		case c.src == c.dst:
			res[p] = Resolution{Side: ResolveDst}
		case !rh.isFactPath(p):
			stateRule(c, "not-a-fact")
		case fm.policy.facts == fact.ConflictsConsensus:
			takeConsensus(c, "")
		case !fieldMerge:
			sitePick(c, "") // facts: off
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
				stateRule(c, rec.Reason)
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

// overlayCallerResolutions lays the caller's own resolutions over what a
// `conflicts` setting decided (mergeOpts.overlayResolutions, the experiment
// commit only): each path the caller named takes the caller's resolution, and
// the setting's record line for it is replaced. A side resolution is recorded
// as a whole-set choice is, `reason=chosen`; a body resolution records no line,
// exactly as a caller body resolution never has (it has no kept side, and the
// merge commit's tree holds what landed).
func overlayCallerResolutions(
	res map[string]Resolution, lines []string, caller map[string]Resolution,
	baseCommit, srcCommit, dstCommit *object.Commit,
) (map[string]Resolution, []string, error) {
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, nil, err
	}
	srcTree, err := srcCommit.Tree()
	if err != nil {
		return nil, nil, err
	}
	dstTree, err := dstCommit.Tree()
	if err != nil {
		return nil, nil, err
	}
	if res == nil {
		res = make(map[string]Resolution, len(caller))
	}
	kept := lines[:0:0]
	for _, l := range lines {
		if _, named := caller[trailerPath(l)]; !named {
			kept = append(kept, l)
		}
	}
	paths := make([]string, 0, len(caller))
	for p := range caller {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		r := caller[p]
		res[p] = r
		if len(r.Body) == 0 && r.Side != "" {
			kept = append(kept, conflictLine(shapeOf(p, baseTree, srcTree, dstTree), r.Side, StrategyRefuse, "chosen"))
		}
	}
	return res, kept, nil
}

// trailerPath is the path a Knomit-Merge / Knomit-Conflict line names.
func trailerPath(line string) string {
	_, rest, _ := strings.Cut(line, ": ")
	path, _, _ := strings.Cut(rest, " ")
	return path
}

// settledFrom reads a merge's record lines back as per-path outcomes (see
// SettledPath). Lines with any other key are skipped.
func settledFrom(lines []string) []SettledPath {
	var out []SettledPath
	for _, l := range lines {
		key, rest, ok := strings.Cut(l, ": ")
		if !ok || (key != TrailerMerge && key != TrailerConflict) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		sp := SettledPath{Path: fields[0]}
		kv := map[string]string{}
		for _, f := range fields[1:] {
			if k, v, ok := strings.Cut(f, "="); ok {
				kv[k] = v
			}
		}
		sp.Dropped = kv["dropped"]
		if key == TrailerMerge {
			// A field merge drops nothing. A delete-vs-edit records the
			// losing edit: the side that deleted is the one that landed.
			switch {
			case strings.HasPrefix(sp.Dropped, "src-"):
				sp.Kept = string(ResolveDst)
			case strings.HasPrefix(sp.Dropped, "dst-"):
				sp.Kept = string(ResolveSrc)
			default:
				sp.Kept = "merged"
			}
			sp.Deleted = kv["decided"] == "delete"
		} else {
			sp.Kept = kv["kept"]
			sp.Chosen = kv["reason"] == "chosen"
			sp.Deleted = sp.Kept != "" && kv[sp.Kept] == "none"
		}
		out = append(out, sp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
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

// conflictsStrategy is the `conflicts` strategy the ontology at branch's tip
// names (conflictsPolicy.strategy), or ok=false when both keys read off there:
// absent without `consensus: auto`, explicit off, a value this build does not
// know, no ontology, no such branch. The repo's owner decides it where every
// instance reads it — the tip of the CONSENSUS branch — so the caller passes
// that branch, never a hardcoded name.
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
				Msg(`conflicts: the ontology's value is unreadable or unknown (this knomit knows an object with facts: off|merge|merge:consensus|consensus and state: off|consensus); read as off`)
		}
		return "", false
	}
	if !cs.On() {
		return "", false
	}
	return conflictsPolicy{facts: cs.Facts, state: cs.State}.strategy(), true
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
