// The judge of this instance's OWN commits before they reach the fleet
// repository's main (user ruling 2026-10-16, item 2: "Fix"). Every other
// judge runs where a MEMBER's branch enters the repository (receive-pack, the
// master's fetch, the merger's re-judge); the master's own writes — root,
// leaves, CRL, revocations, host record, master marker, its own record, the
// merges of members' branches — reached main without one. Now every store
// path that moves or publishes the fleet's main from this instance runs
// judgeOwnRange over the commits it adds, BEFORE main moves:
//
//   - AdvanceLocalUpstream (the hub's local reconcile, create, Host here, a
//     hub claim and stand-down, and local main following a forge publish);
//   - PublishUpstream (a forge-hosted master's publish of main);
//   - PublishUpstreamChange (a forge claim, stand-down or decommission: the
//     one commit it builds on main's tip);
//   - PublishTo (the master's seed of a forge it moves the fleet onto).
//
// A refusal moves nothing and is an *OwnCommitRefusedError; the repos layer
// records it as the fleet's last error (GET /fleet last_error).
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// OwnCommitRefusedText is the part of every *OwnCommitRefusedError's text
// that says main was held back by the judge ("knomit: main was not moved:
// commit <hash> refused by ..."): the fleet's recorded last error carries it
// until main moves again.
const OwnCommitRefusedText = "was not moved: commit"

// ErrOwnCommitRefused matches *OwnCommitRefusedError with errors.Is.
var ErrOwnCommitRefused = errors.New("main was not moved: a commit of this instance's own branch is refused")

// OwnCommitRefusedError: judgeOwnRange refused a commit this instance was
// about to put on its main. Nothing moved. Text names the commit, the rule
// and the reason in the shape a member's refusal has ("commit <hash> refused
// by verify_signatures=enforce: <rule>: <reason>", or CommitRefusedByValidation).
type OwnCommitRefusedError struct {
	Upstream string
	Commit   string
	Rule     string
	Text     string
}

func (e *OwnCommitRefusedError) Error() string {
	return fmt.Sprintf("knomit: %s was not moved: %s", e.Upstream, e.Text)
}

func (e *OwnCommitRefusedError) Is(target error) bool { return target == ErrOwnCommitRefused }

// judgeOwnRange judges the commits of tip ∖ the history of base (the
// consensus branch's tip; zero when there is none yet) that this instance is
// about to put on main. own is the agent id this instance authors as.
//
// It runs only where the judged ontology is the FLEET ontology: the ontology
// at base, or, while base holds none (the first publish of a fleet created
// onto a forge whose main held only a README), at tip. Every other repository
// returns nil at once: its main follows its own agent branch unjudged (the
// ontology's validations bind those writes at the MCP write path, not here).
//
// The rules, for every commit of the range:
//  1. RuleValidation (every mode), on the range's NET change
//     (branchFactsValid: tip against base and their merge base): each fact
//     it adds or changes keeps the ontology's validations,
//     members-are-policy included. A refusal names the commit that wrote the
//     failing content (introducedBy), the one a Drop must name.
//  2. judgeCommit. A commit NOT authored by own is a member's, merged in: it
//     is judged as at its entry, against the fleet at base (at tip while
//     base holds no root), with entry set so host-record refuses a member's
//     change of the host record or the marker, and no signer rule (it has no
//     pusher here; signer = pusher held at its push). A commit authored by
//     own is admitted when judgeCommit admits it against the fleet at base
//     OR at vouch (this instance's agent branch tip, the state it is
//     publishing; zero = tip): the founder's root, leaf and record are not
//     on main before create publishes, and a member that took the root over
//     may claim before any master merged its join. Every rule that does not
//     read the fleet revision (malformed-tree, not-owner, the artifact rules)
//     gives the same verdict either way. For an own commit the record-owner
//     rule also admits the ontology file (judge.ownOntology): the creator
//     writes it and the master refreshes it; nothing else outside its own
//     folder and the declared artifacts.
//  3. A commit authored by own: signer = self (RuleNotPusher, the hub's
//     signer = pusher with this instance as the pusher), and the master's
//     side of host-record (ownHostRecord).
//
// verify_signatures at the judged ontology decides 2 and 3 as everywhere:
// enforce refuses, log logs, off checks nothing.
func (rh *repoHandler) judgeOwnRange(upstream string, base, tip plumbing.Hash, own string, vouch plumbing.Hash) error {
	tipC, err := object.GetCommit(rh.gits, tip)
	if err != nil {
		return fmt.Errorf("judge own commits: %s: %w", tip, err)
	}
	var baseC *object.Commit
	onBase := map[plumbing.Hash]bool{}
	if !base.IsZero() {
		if baseC, err = object.GetCommit(rh.gits, base); err != nil {
			return fmt.Errorf("judge own commits: %s: %w", base, err)
		}
		if err := walkHistory(rh.gits, base, nil, func(c *object.Commit) { onBase[c.Hash] = true }); err != nil {
			return fmt.Errorf("judge own commits: %w", err)
		}
	}
	ontC := baseC
	raw := []byte(nil)
	if baseC != nil {
		if raw, err = treeOntology(baseC); err != nil {
			return fmt.Errorf("judge own commits: the ontology at %s: %w", base, err)
		}
	}
	if raw == nil {
		ontC = tipC
		if raw, err = treeOntology(tipC); err != nil {
			return fmt.Errorf("judge own commits: the ontology at %s: %w", tip, err)
		}
	}
	if raw == nil {
		return nil
	}
	ont, err := fact.ParseOntology(raw)
	if err != nil {
		return fmt.Errorf("judge own commits: the ontology at %s: %w", ontC.Hash, err)
	}
	if !fact.IsFleetOntology(ont) {
		return nil
	}
	var commits []*object.Commit
	if err := walkHistory(rh.gits, tip, onBase, func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return fmt.Errorf("judge own commits: %w", err)
	}
	if len(commits) == 0 {
		return nil
	}
	root := rh.ontologyRoot()
	if r, ok := branchFactsValid(tipC, baseC, ont, root); !ok {
		return &OwnCommitRefusedError{Upstream: upstream, Commit: r.Commit, Rule: r.Rule,
			Text: fmt.Sprintf("commit %s %s: %s: %s", r.Commit, CommitRefusedByValidation, r.Rule, r.Reason)}
	}
	vs, err := fact.ReadVerifySettings(raw)
	if err != nil {
		return fmt.Errorf("judge own commits: %w", err)
	}
	if !vs.Valid {
		return errors.New("judge own commits: verify_signatures has an unknown value")
	}
	if vs.Mode == VerifyOff {
		return nil
	}
	// The members' commits are judged against the fleet at base (main), as
	// at their entry; while base holds no fleet ontology or no root (create),
	// at tip.
	var fleet Fleet
	if ontC == baseC {
		if fleet, err = loadFleetAt("own", baseC, root); err != nil {
			return fmt.Errorf("judge own commits: the fleet at %s: %w", base, err)
		}
	}
	if ontC != baseC || fleet.PKI == nil {
		if fleet, err = loadFleetAt("own", tipC, root); err != nil {
			return fmt.Errorf("judge own commits: the fleet at %s: %w", tip, err)
		}
	}
	// This instance's own commits may also be vouched for by the state it is
	// publishing: its agent branch's tip (vouch; zero = tip). That is where
	// its own record is while main does not hold it yet — a founder before
	// main exists, a member that took the root over before any master merged
	// its join — exactly as `knomit verify ci --fleet-rev` the published
	// main judges them.
	if vouch.IsZero() {
		vouch = tip
	}
	vouchC, err := object.GetCommit(rh.gits, vouch)
	if err != nil {
		return fmt.Errorf("judge own commits: %s: %w", vouch, err)
	}
	vouched, err := loadFleetAt("own", vouchC, root)
	if err != nil {
		return fmt.Errorf("judge own commits: the fleet at %s: %w", vouch, err)
	}
	onMain := ""
	if baseC != nil {
		if onMain, err = markerIn(baseC); err != nil {
			return fmt.Errorf("judge own commits: the master marker at %s: %w", base, err)
		}
	}
	selfFP := ""
	if s, err := rh.commitSigner(); err == nil {
		if fp, err := keyFingerprint(s.PublicKey()); err == nil {
			selfFP = fp
		}
	}
	member := newJudge(fleet, root, ont, root)
	member.entry = true
	mine := newJudge(fleet, root, ont, root)
	mine.ownOntology = true
	mineVouched := newJudge(vouched, root, ont, root)
	mineVouched.ownOntology = true
	for _, c := range commits {
		id := agentIDOfAuthor(c.Author.Email)
		var r Refusal
		var ok bool
		if id != own || own == "" {
			r, ok = member.judgeCommit(c)
		} else {
			if r, ok = mine.judgeCommit(c); !ok {
				if rv, okv := mineVouched.judgeCommit(c); okv {
					r, ok = rv, okv
				}
			}
			if ok {
				r, ok = signerIsPusher(c, selfFP)
			}
			if ok {
				r, ok = ownHostRecord(c, own, onMain)
			}
		}
		if ok {
			continue
		}
		if vs.Mode == VerifyEnforce {
			return &OwnCommitRefusedError{Upstream: upstream, Commit: c.Hash.String(), Rule: r.Rule,
				Text: fmt.Sprintf("commit %s %s: %s: %s", c.Hash, CommitRefusedByJudge, r.Rule, r.Reason)}
		}
		log.Warn().Str("repo", rh.name).Str("commit", c.Hash.String()).Str("rule", r.Rule).Str("reason", r.Reason).
			Msg("judge own commits: commit would be refused (verify_signatures=log)")
	}
	return nil
}

// ownHostRecord is the host-record rule for this instance's OWN commits
// (judgeOwnRange). A member's branch may never change artifacts/fleet/host
// or artifacts/fleet/master (hostRecord); the master writes both, so for a
// commit authored by own (signed by own's key: signerIsPusher ran first) the
// rule admits exactly the master's legitimate writes and refuses anything
// else. With onMain the agent id main's marker names ("" for none):
//   - a commit that changes neither path is not this rule's;
//   - a commit that changes the marker may leave it naming ITS AUTHOR or
//     delete it, never name another agent (create, a claim and Host here
//     name themselves; stand-down and a forge decommission delete it);
//   - when main names ANOTHER agent, a commit that changes either path is
//     admitted only when the marker it leaves names its author: that is Host
//     here taking the fleet over (its marker commit comes first, so its host
//     record commit sees itself named). Otherwise a root holder main does not
//     name would be rewriting the acting master's record.
//
// When main names own, or nobody (create, a candidate's claim, a fleet from
// before the marker), the host record is own's to write.
func ownHostRecord(c *object.Commit, own, onMain string) (Refusal, bool) {
	refuse := func(reason string) (Refusal, bool) {
		return Refusal{Commit: c.Hash.String(), Rule: RuleHostRecord, Reason: reason}, false
	}
	host, marker, err := changesHostRecord(c)
	if err != nil {
		return refuse(err.Error())
	}
	if !host && !marker {
		return Refusal{}, true
	}
	after, err := markerIn(c)
	if err != nil {
		return refuse(fmt.Sprintf("the master marker it leaves does not parse: %v", err))
	}
	if marker && after != "" && after != own {
		return refuse(fmt.Sprintf("agent %q writes %s naming %q: the marker names only the agent that writes it", own, fact.FleetMasterPath, after))
	}
	if onMain != "" && onMain != own && after != own {
		return refuse(fmt.Sprintf("agent %q changes the fleet's host record or master marker while main names %q as the acting master", own, onMain))
	}
	return Refusal{}, true
}

// changesHostRecord reports whether c changes, against ANY parent and in any
// case spelling, the host record (fact.FleetHostPath) and the master marker
// (fact.FleetMasterPath).
func changesHostRecord(c *object.Commit) (host, marker bool, err error) {
	tree, err := c.Tree()
	if err != nil {
		return false, false, fmt.Errorf("the commit's tree cannot be read: %v", err)
	}
	parents, err := parentTrees(c)
	if err != nil {
		return false, false, fmt.Errorf("a parent's tree cannot be read: %v", err)
	}
	for _, pt := range parents {
		changes, err := object.DiffTree(pt, tree)
		if err != nil {
			return false, false, fmt.Errorf("diff: %v", err)
		}
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				host = host || strings.EqualFold(name, fact.FleetHostPath)
				marker = marker || strings.EqualFold(name, fact.FleetMasterPath)
			}
		}
	}
	return host, marker, nil
}

// markerIn is the agent id the master marker in c's tree names, "" when it
// holds none.
func markerIn(c *object.Commit) (string, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	f, err := tree.File(fact.FleetMasterPath)
	if errors.Is(err, object.ErrFileNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	raw, err := f.Contents()
	if err != nil {
		return "", err
	}
	return fact.ParseFleetMaster([]byte(raw))
}

// publishBase is what a publish of upstream to the origin is judged against
// (judgeOwnRange's base): the origin's main as this copy last fetched it
// (refs/remotes/origin/<upstream>), else the local upstream, else nothing.
func (ri *remoteIndex) publishBase(upstream string) plumbing.Hash {
	for _, name := range []plumbing.ReferenceName{plumbing.NewRemoteReferenceName("origin", upstream), plumbing.NewBranchReferenceName(upstream)} {
		if ref, err := ri.rh.gits.Reference(name); err == nil {
			return ref.Hash()
		}
	}
	return plumbing.ZeroHash
}

// ownID is the agent id this instance authors commits on branch as ("" when
// it has none: then no commit is its own, and every one is judged as a
// member's).
func (ri *remoteIndex) ownID(branch string) string {
	id, _ := ri.rh.ownAgentID(branch)
	return id
}

// branchTip is branch's tip, zero when there is none.
func (ri *remoteIndex) branchTip(branch string) plumbing.Hash {
	if branch == "" {
		return plumbing.ZeroHash
	}
	ref, err := ri.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

// ErrNotUnpublished: DropUnpublishedCommit was asked to drop a commit that is
// not on the branch, or that the consensus branch already holds.
var ErrNotUnpublished = errors.New("the commit is not an unpublished commit of this branch")

// ErrDropMerge: DropUnpublishedCommit was asked to drop a merge commit. A
// merge brings in another branch's commits; the commit that wrote the
// content is the one to drop (introducedBy names a non-merge commit when one
// wrote it).
var ErrDropMerge = errors.New("a merge commit cannot be dropped: drop the commit that wrote the content")

// DropForeignError: dropping the commit would replay ANOTHER agent's commit
// outside the merge that brought it (the merge carries the dropped commit),
// re-signing it with this instance's key, which the fleet's judge refuses.
// Returned only when the caller asked to keep signatures intact (ownOnly).
type DropForeignError struct {
	Commit, Agent, Merge string
}

func (e *DropForeignError) Error() string {
	return fmt.Sprintf("dropping it would re-sign agent %q's commit %s, which came in with merge %s", e.Agent, e.Commit, e.Merge)
}

// DropUnpublishedCommit is the operator's way out of a refusal by the judge
// of this instance's own commits (judgeOwnRange): the refused commit stays
// on the agent branch, so every later move of main is refused too, and
// knomit never rewinds a branch by itself. It rebuilds branch on the
// consensus branch's tip, replaying in order every commit of branch ∖ main
// on the first-parent line (re-signed by this instance, its own content
// winning a conflict) EXCEPT drop, so nothing but drop's own change is lost:
//   - a merge commit is replayed as a merge of the same other parents, so
//     what it brought in stays (a member's branch the merger had merged, a
//     committed experiment);
//   - a merge whose other parents reach drop (it brought drop in, or its
//     side forked after drop) is unfolded: its side's own non-merge commits
//     are replayed one by one, drop excluded (counted in mergesDropped). A
//     conflict resolution the side's own merges made is not kept. With
//     ownOnly (the fleet, whose judge refuses a commit re-signed for another
//     agent) a side commit authored by another agent refuses the drop
//     (*DropForeignError) instead of re-signing it;
//   - where a later commit edited a file drop wrote, the later version is
//     kept, whole (#533 review S3).
//
// drop must be a non-merge commit of branch that main does not hold
// (ErrNotUnpublished, ErrDropMerge). Returns how many commits were replayed
// and how many merges were unfolded.
func (s *Service) DropUnpublishedCommit(ctx context.Context, branch string, drop plumbing.Hash, ownOnly bool) (replayed, mergesDropped int, err error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	if upstream == "" || branch == "" || branch == upstream {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %q is not a branch to rebuild on %q", branch, upstream)
	}
	unlock := rh.lockBranch(branch)
	defer unlock()
	localRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %s: %w", branch, err)
	}
	upRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(upstream))
	if err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %s: %w", upstream, err)
	}
	onUp := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, upRef.Hash(), nil, func(c *object.Commit) { onUp[c.Hash] = true }); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	unpublished := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, localRef.Hash(), onUp, func(c *object.Commit) { unpublished[c.Hash] = true }); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if !unpublished[drop] {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %s on %s: %w", drop, branch, ErrNotUnpublished)
	}
	dropC, err := object.GetCommit(rh.gits, drop)
	if err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if dropC.NumParents() > 1 {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %s: %w", drop, ErrDropMerge)
	}
	own, _ := rh.ownAgentID(branch)
	type step struct {
		c       *object.Commit
		asMerge bool
	}
	// groups in tip -> base order; each group's steps in replay order.
	var groups [][]step
	cur, err := object.GetCommit(rh.gits, localRef.Hash())
	if err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	reaches := func(from []plumbing.Hash, stop map[plumbing.Hash]bool, target plumbing.Hash) (bool, error) {
		found := false
		for _, p := range from {
			if err := walkHistory(rh.gits, p, stop, func(c *object.Commit) {
				if c.Hash == target {
					found = true
				}
			}); err != nil {
				return false, err
			}
		}
		return found, nil
	}
	for !onUp[cur.Hash] {
		switch {
		case cur.NumParents() > 1:
			// A merge whose other parents reach drop (it brought drop in, or
			// its side forked after drop) cannot be replayed as a merge: its
			// history would carry drop again. Its side's OWN commits are
			// replayed one by one instead, drop excluded (#533 re-review S1),
			// so nothing but drop's own change is lost. Every other merge is
			// replayed as a merge of the same parents (#533 review S1).
			carries, err := reaches(cur.ParentHashes[1:], onUp, drop)
			if err != nil {
				return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
			}
			if !carries {
				groups = append(groups, []step{{c: cur, asMerge: true}})
				break
			}
			mergesDropped++
			first := map[plumbing.Hash]bool{}
			for h := range onUp {
				first[h] = true
			}
			if err := walkHistory(rh.gits, cur.ParentHashes[0], onUp, func(c *object.Commit) { first[c.Hash] = true }); err != nil {
				return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
			}
			var side []step
			var werr error
			for _, p := range cur.ParentHashes[1:] {
				if err := walkHistory(rh.gits, p, first, func(c *object.Commit) {
					first[c.Hash] = true // a commit two parents share once
					if c.Hash == drop || c.NumParents() > 1 {
						return
					}
					// Replayed outside its merge, the commit is re-signed by this
					// instance. Where signatures are judged (the fleet), another
					// agent's commit cannot be: refuse instead of losing it.
					if ownOnly && werr == nil {
						if id := agentIDOfAuthor(c.Author.Email); id != own {
							werr = &DropForeignError{Commit: c.Hash.String(), Agent: id, Merge: cur.Hash.String()}
						}
					}
					side = append(side, step{c: c})
				}); err != nil {
					return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
				}
			}
			if werr != nil {
				return 0, 0, werr
			}
			groups = append(groups, side)
		case cur.Hash != drop:
			groups = append(groups, []step{{c: cur}})
		}
		if cur.NumParents() == 0 {
			break
		}
		if cur, err = cur.Parent(0); err != nil {
			return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
		}
	}
	var line []step
	for i := len(groups) - 1; i >= 0; i-- {
		line = append(line, groups[i]...)
	}
	tempRef := plumbing.NewBranchReferenceName(branch + "-dropping")
	defer func() { _ = rh.gits.RemoveReference(tempRef) }()
	current := upRef.Hash()
	for _, st := range line {
		var others []plumbing.Hash
		if st.asMerge {
			others = st.c.ParentHashes[1:]
		}
		if current, err = rh.replayCommit(ctx, st.c, current, StrategyLocalWins, others...); err != nil {
			return 0, 0, fmt.Errorf("DropUnpublishedCommit: replay %s: %w", st.c.Hash, err)
		}
		if err := rh.gits.SetReference(plumbing.NewHashReference(tempRef, current)); err != nil {
			return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
		}
	}
	ctx = context.WithoutCancel(ctx)
	if err := rh.deriveBeforeAdvance(ctx, current); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), current)); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.repopulateBranch(ctx, branch); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: repopulate: %w", err)
	}
	if err := rh.notifyCommit(ctx, branch, current); err != nil {
		return 0, 0, fmt.Errorf("DropUnpublishedCommit: notify: %w", err)
	}
	log.Warn().Str("branch", branch).Str("dropped", drop.String()).Int("replayed", len(line)).Int("merges_unfolded", mergesDropped).
		Msg("DropUnpublishedCommit: the branch was rebuilt on its upstream without the dropped commit")
	return len(line), mergesDropped, nil
}
