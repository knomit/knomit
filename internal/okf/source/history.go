package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"knomit/internal/fact"
	"knomit/internal/okf"
)

// maxCommits bounds the history walk. A var rather than a const so a test can
// lower it: authoring 5000 commits to reach the bound would cost far more than
// the behaviour at the bound is worth, and that behaviour now has to be proven
// rather than assumed.
var maxCommits = 5000

// okfHistoryResult is one pass over the commit DAG: the changelog entries, the
// per-path authoring times, and the per-path revision lists. All three come
// from the same walk, so they are returned together rather than recomputed.
type okfHistoryResult struct {
	Events    []okf.LogEntry
	Authored  map[string]time.Time
	Revisions map[string][]okf.Revision
	// Retired is every fact the knowledge base has withdrawn, newest first:
	// a path deleted somewhere in the history and still absent from the source
	// commit's tree. Published as an index only — see okf.Retirement.
	Retired  []okf.Retirement
	Warnings []string
}

// okfHistory walks commits from sourceSHA (bounded), producing log entries, a
// path→authoring-time map, and each path's revision list. Authoring time is
// the OLDEST commit that touched a path; an Update entry is emitted for each
// later commit that modified it. Deterministic per sourceSHA. Bounded to avoid
// unbounded walks on huge DAGs.
//
// Hitting the bound DEGRADES THE PUBLISHED BUNDLE, so it is reported through
// Warnings rather than absorbed silently. Three things go wrong past it, and
// none of them is visible in the output they corrupt:
//
//   - a fact whose every revision fell off the walk has no authoring time, and
//     okfReadFacts stamps it with the export commit's — a 2024 fact publishes
//     today's date as `timestamp` and `generated.at`;
//   - retirements older than the bound vanish from views/retired.md;
//   - it is not stable over time. Commits accrue at the tip and push older ones
//     off the back, so a date already published can silently CHANGE on a later
//     sync.
func okfHistory(st storer.EncodedObjectStorer, sourceSHA plumbing.Hash, p Progress) (okfHistoryResult, error) {

	root, err := object.GetCommit(st, sourceSHA)
	if err != nil {
		return okfHistoryResult{}, fmt.Errorf("okf: get source commit: %w", err)
	}

	authored := map[string]time.Time{} // path -> earliest touch time
	revisions := map[string][]okf.Revision{}
	// retired holds only the LATEST retirement per path, so a
	// delete/recreate/delete sequence is reported once rather than twice.
	retired := map[string]okf.Retirement{}
	var events []okf.LogEntry
	// eventRev[i] locates events[i]'s revision, so log.md can report the same
	// delta the fact's own # History does. See the append site below.
	var eventRev []revisionRef

	iter := object.NewCommitPreorderIter(root, nil, nil)
	seenCommits := 0
	truncated := false
	err = iter.ForEach(func(c *object.Commit) error {
		if seenCommits >= maxCommits {
			// Reached only when a commit BEYOND the bound exists, so this is a
			// true truncation rather than a history that happens to end here.
			truncated = true
			return object.ErrCanceled
		}
		seenCommits++
		if p != nil {
			p("commits", seenCommits)
		}

		changed, deleted, err := okfChangedFactPaths(c)
		if err != nil {
			return err
		}
		for _, d := range deleted {
			kind, successor := okfParseRetirement(c.Message)
			r := okf.Retirement{
				Date:          c.Committer.When,
				Title:         d.title,
				Path:          d.path,
				Kind:          kind,
				SuccessorPath: successor,
			}
			// Strictly-after keeps the first record seen at an identical
			// timestamp. The preorder walk runs tip-first and is deterministic,
			// so that tie-break is stable across runs.
			if prev, seen := retired[d.path]; !seen || r.Date.After(prev.Date) {
				retired[d.path] = r
			}
		}
		for _, ch := range changed {
			if prev, seen := authored[ch.path]; !seen || c.Committer.When.Before(prev) {
				authored[ch.path] = c.Committer.When
			}
			kind := "Update"
			if ch.created {
				kind = "Creation"
			}
			// An event and a revision are appended together, always, so the two
			// stay index-aligned per path. Recording that index is what lets each
			// event pick up its delta below without matching on timestamps — the
			// one key that is NOT unique here, since a batch commit stamps every
			// path it touched with the same second.
			eventRev = append(eventRev, revisionRef{path: ch.path, tipIdx: len(revisions[ch.path])})
			events = append(events, okf.LogEntry{
				Date:  c.Committer.When,
				Kind:  kind,
				Title: ch.title,
				Path:  ch.path,
			})
			revisions[ch.path] = append(revisions[ch.path], okf.Revision{
				Date:       c.Committer.When,
				Operation:  okfOperationLabel(c.Author.Email),
				Confidence: ch.confidence,
				Title:      ch.title,
				BodyDigest: ch.bodyDigest,
				RefCount:   ch.refCount,
			})
		}
		return nil
	})
	if err != nil && err != object.ErrCanceled {
		return okfHistoryResult{}, err
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
	// The preorder walk starts at the tip and moves toward the root, so each
	// path's revisions accumulated newest-first above. renderHistory's mapper
	// contract requires oldest-first input — it relies on caller order to
	// break same-timestamp ties chronologically — so reverse every slice here.
	for _, rs := range revisions {
		for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
			rs[i], rs[j] = rs[j], rs[i]
		}
	}

	// Give each event the delta its revision earned, so log.md and the fact's
	// own # History agree by construction rather than by two implementations
	// happening to compute the same thing. An event whose revision was NOT
	// retained keeps an empty Delta, which is how RenderLog drops it.
	stampEventDeltas(events, eventRev, revisions)

	// A fact deleted and later written again is LIVE, not retired: only paths
	// ABSENT from the source commit's tree may be published as withdrawn.
	// Filtering here rather than at collection time also neutralises merge
	// artifacts — a path a merge appears to drop but the tree still holds is
	// simply not in the result.
	rootTree, err := root.Tree()
	if err != nil {
		return okfHistoryResult{}, fmt.Errorf("okf: source tree: %w", err)
	}
	live := map[string]bool{}
	if err := rootTree.Files().ForEach(func(f *object.File) error {
		live[f.Name] = true
		return nil
	}); err != nil {
		return okfHistoryResult{}, err
	}
	retiredList := make([]okf.Retirement, 0, len(retired))
	for p, r := range retired {
		if live[p] {
			continue
		}
		retiredList = append(retiredList, r)
	}
	// Newest first, with a total order on ties: the mapper is pure but this
	// slice is assembled from a map, so it must be sorted to be deterministic.
	sort.Slice(retiredList, func(i, j int) bool {
		a, b := retiredList[i], retiredList[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date)
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.Path < b.Path
	})

	var warnings []string
	if truncated {
		warnings = append(warnings, fmt.Sprintf(
			"history walk stopped at the %d-commit bound: revisions, creation dates and retirements older than that are missing from this bundle, and facts with no surviving revision are stamped with the export commit's date",
			maxCommits))
	}

	return okfHistoryResult{
		Events:    events,
		Authored:  authored,
		Revisions: revisions,
		Retired:   retiredList,
		Warnings:  warnings,
	}, nil
}

// revisionRef locates one event's revision: the path it belongs to, and its
// index in that path's TIP-FIRST accumulation order.
type revisionRef struct {
	path   string
	tipIdx int
}

// stampEventDeltas copies each retained revision's delta onto its event.
//
// revisions have been reversed to oldest-first by now, so a tip-first index i
// over n revisions is oldest-first index n-1-i. Deriving the delta from the
// revision the event was recorded WITH — rather than re-deriving it from dates —
// is what makes log.md's claim about a commit identical to the one the fact's
// own # History makes about it.
func stampEventDeltas(events []okf.LogEntry, refs []revisionRef, revisions map[string][]okf.Revision) {
	deltaAt := make(map[string]map[int]string, len(revisions))
	for p, rs := range revisions {
		keep, deltas := okf.MeaningfulRevisions(rs)
		m := make(map[int]string, len(keep))
		for i, k := range keep {
			m[k] = deltas[i]
		}
		deltaAt[p] = m
	}
	for i := range events {
		if i >= len(refs) {
			return // defensive: the two are appended together and cannot diverge
		}
		r := refs[i]
		oldest := len(revisions[r.path]) - 1 - r.tipIdx
		if oldest == 0 && events[i].Kind != "Creation" {
			// Revision 0 is the creation slot, and MeaningfulRevisions labels it
			// "created" because there is nothing before it to diff against. When
			// the EVENT there is not this path's Creation — its real one fell
			// outside the walk's bound, or the path was deleted and written again
			// — that label is a lie the log would print as "**Update** … —
			// created". We know it changed and cannot say how, which is precisely
			// what an empty Delta means and what RenderLog drops.
			continue
		}
		events[i].Delta = deltaAt[r.path][oldest]
	}
}

// okfRetirementVerbs are the structural forms knomit writes into the subject of
// a retirement commit. knomit records no free-text reason anywhere, so these
// two prefixes are the entire signal — nothing else may be inferred.
const (
	okfSubsumedPrefix = "subsumed by "
	okfRetractPrefix  = "retract "
)

// okfParseRetirement classifies a deletion from its commit message.
//
//	"<op>: subsumed by kb/<path>" ⇒ superseded, successor is that path
//	"<op>: retract kb/<path>"     ⇒ retracted
//	anything else                 ⇒ retracted, no successor
//
// "superseded" means REPLACED BY A NAMED FACT, matching the vocabulary
// knomit_explain uses. A message that claims subsumption without naming a kb/
// path (e.g. "subsumed by distilled fact") names no successor, so it cannot be
// reported as superseded — inventing one would put a link in the index that no
// commit ever recorded.
func okfParseRetirement(message string) (kind, successorPath string) {
	subject := message
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	subject = strings.TrimSpace(subject)

	i := strings.Index(subject, ": ")
	if i < 0 {
		return okf.RetiredRetracted, ""
	}
	rest := strings.TrimSpace(subject[i+len(": "):])
	if p, ok := strings.CutPrefix(rest, okfSubsumedPrefix); ok {
		// "subsumed by" means the fact was folded into a better statement —
		// superseded — whether or not the successor is a named path. Some
		// subsumptions name their replacement ("subsumed by kb/…"), others
		// only describe it ("subsumed by distilled fact"). Both are
		// supersessions; only the former can be linked. Reporting the
		// unnamed ones as "retracted" would tell a reader the claim was
		// withdrawn as wrong, when it was actually absorbed.
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, okfOntologyRoot+"/") && strings.HasSuffix(p, ".md") {
			return okf.RetiredSuperseded, p
		}
		return okf.RetiredSuperseded, ""
	}
	return okf.RetiredRetracted, ""
}

type okfChange struct {
	path    string
	title   string
	created bool // the path was absent from the parent tree (an Insert)

	// Snapshot of the fact AT THIS REVISION, for the History deltas.
	confidence float64
	bodyDigest string
	refCount   int
}

// okfDeletion is a kb/*.md path the commit removed, with the fact's title read
// from the blob it removed — the last place that text exists, since after the
// deletion the path holds nothing.
type okfDeletion struct {
	path  string
	title string
}

// okfChangedFactPaths returns the kb/*.md paths added or modified by commit c
// relative to its first parent, and separately the ones it deleted. For a root
// (parentless) commit, every kb/*.md file in its tree is treated as a creation
// and nothing is deleted.
func okfChangedFactPaths(c *object.Commit) ([]okfChange, []okfDeletion, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, nil, err
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
			return nil, nil, err
		}
		return out, nil, nil
	}

	parent, err := c.Parent(0)
	if err != nil {
		return nil, nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, nil, err
	}
	changes, err := parentTree.Diff(tree)
	if err != nil {
		return nil, nil, err
	}

	var out []okfChange
	var deleted []okfDeletion
	for _, ch := range changes {
		from, to, err := ch.Files()
		if err != nil {
			return nil, nil, err
		}
		if to == nil {
			// A deletion. The retirement's title comes from the blob being
			// removed, and its kind/successor from THIS commit's message — so a
			// merge that merely carries someone else's deletion across must be
			// ignored, or the merge subject ("Merge pull request #3 …") would
			// overwrite the structural classification the real commit recorded.
			if from == nil || absentInAnotherParent(c, ch.From.Name) {
				continue
			}
			if d, ok := okfDeletionFromFile(ch.From.Name, func() (string, error) { return from.Contents() }); ok {
				deleted = append(deleted, d)
			}
			continue
		}
		// A merge that carries a file in unchanged from another parent is not an
		// edit. Diffing against the FIRST parent alone reports such a file as
		// added or modified, which would manufacture a revision for a fact
		// nobody touched — the dominant source of phantom history on a branch
		// that reconciles often.
		if unchangedInAnotherParent(c, ch.To.Name, to.Hash) {
			continue
		}
		// ch.To.Name is the full tree path (e.g. "kb/decisions/.../x.md");
		// to.Name from Files() is only the basename, so it cannot be used for
		// the ontology-prefix filter. from == nil means the path was absent
		// from the parent tree — an Insert, i.e. a Creation.
		if change, ok := okfChangeFromFile(ch.To.Name, from == nil, func() (string, error) { return to.Contents() }); ok {
			out = append(out, change)
		}
	}
	return out, deleted, nil
}

// absentInAnotherParent reports whether path was ALREADY gone in one of c's
// OTHER parents, meaning c merely merged in a deletion someone else made. The
// mirror image of unchangedInAnotherParent, and needed for the same reason:
// diffing against the first parent alone attributes a branch's retirement to
// the merge commit, whose message carries none of knomit's structural
// vocabulary — which would silently downgrade every superseded fact reconciled
// through a merge to a bare "retracted". The originating commit is visited by
// the same DAG walk, so nothing is lost by skipping it here.
func absentInAnotherParent(c *object.Commit, path string) bool {
	if c.NumParents() < 2 {
		return false
	}
	for i := 1; i < c.NumParents(); i++ {
		p, err := c.Parent(i)
		if err != nil {
			continue
		}
		if _, err := p.File(path); err != nil {
			return true // absent in this parent
		}
	}
	return false
}

// okfDeletionFromFile builds an okfDeletion for a removed kb/*.md path. It
// returns ok=false for paths outside the ontology and for content that is not a
// parseable fact — non-fact markdown under kb/ was never exported, so its
// removal is not a retirement of knowledge.
func okfDeletionFromFile(name string, contents func() (string, error)) (okfDeletion, bool) {
	if !strings.HasPrefix(name, okfOntologyRoot+"/") || !strings.HasSuffix(name, ".md") {
		return okfDeletion{}, false
	}
	content, err := contents()
	if err != nil {
		return okfDeletion{}, false
	}
	f, err := fact.ParseFact(name, content)
	if err != nil {
		return okfDeletion{}, false
	}
	return okfDeletion{path: name, title: f.Title}, true
}

// unchangedInAnotherParent reports whether path already had exactly this blob
// in one of c's OTHER parents, meaning c merely merged an existing version
// rather than changing it. Only merges can satisfy this: a single-parent
// commit's diff already proves the blob differs from its only parent.
func unchangedInAnotherParent(c *object.Commit, path string, blob plumbing.Hash) bool {
	if c.NumParents() < 2 {
		return false
	}
	for i := 1; i < c.NumParents(); i++ {
		p, err := c.Parent(i)
		if err != nil {
			continue
		}
		f, err := p.File(path)
		if err != nil {
			continue // absent in this parent
		}
		if f.Hash == blob {
			return true
		}
	}
	return false
}

// okfChangeFromFile builds an okfChange for a kb/*.md path, reading the fact's
// title and its per-revision snapshot (confidence, body digest, ref count)
// best-effort. created reports whether the path was newly added by the commit.
// It returns ok=false for paths outside the ontology or non-.md files.
//
// The body is reduced to a short digest rather than retained: the History
// deltas only ever ask whether the body CHANGED, so holding revision bodies
// for a whole corpus would be pure waste.
func okfChangeFromFile(name string, created bool, contents func() (string, error)) (okfChange, bool) {
	if !strings.HasPrefix(name, okfOntologyRoot+"/") || !strings.HasSuffix(name, ".md") {
		return okfChange{}, false
	}
	ch := okfChange{path: name, created: created}
	if content, err := contents(); err == nil {
		if f, err := fact.ParseFact(name, content); err == nil {
			ch.title = f.Title
			ch.confidence = f.Confidence
			ch.refCount = len(f.Refs)
			sum := sha256.Sum256([]byte(f.Body))
			ch.bodyDigest = hex.EncodeToString(sum[:8])
		}
	}
	return ch, true
}

// okfAgentEmailDomain is the address suffix knomit's own agents commit under.
const okfAgentEmailDomain = "@agents.knomit.io"

// okfOperationLabel names what a commit did, for the History line.
//
// knomit encodes the operation in the author address as
// "<agent>+<op>@agents.knomit.io". When there is no "+op" suffix, a non-agent
// address means a person committed directly.
//
// This label is DISPLAY ONLY. It must not feed generated.by or OKF's "human:"
// actor convention: it is evidence of who committed, not evidence that anyone
// reviewed the claim, and consumers derive trust tiers from the latter.
func okfOperationLabel(email string) string {
	if op := parseOperation(email); op != "" {
		return op
	}
	if email == "" || strings.HasSuffix(email, okfAgentEmailDomain) {
		return "edit"
	}
	return "human"
}

// parseOperation extracts the "+op" suffix knomit encodes in a commit author's
// local part ("<agent>+<op>@agents.knomit.io"). Copied from internal/store
// rather than imported: store is being stripped of OKF concerns, and this
// package must not depend on it.
func parseOperation(email string) string {
	plusIdx := strings.IndexByte(email, '+')
	if plusIdx < 0 {
		return ""
	}
	atIdx := strings.IndexByte(email, '@')
	if atIdx < 0 || atIdx < plusIdx {
		return ""
	}
	return email[plusIdx+1 : atIdx]
}
