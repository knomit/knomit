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
// wrote it). The error is a *DropMergeError carrying what the confirmed
// fallback, leaving the whole merge out, would lose.
var ErrDropMerge = errors.New("a merge commit cannot be dropped: drop the commit that wrote the content")

// LostCommit is one commit the confirmed fallback (DropUnpublishedCommitLeavingOutMerges)
// leaves out: who wrote it and what it says it does.
type LostCommit struct {
	Commit  string `json:"commit"`
	Agent   string `json:"agent,omitempty"` // the agent id of an agent author, else ""
	Author  string `json:"author"`
	Subject string `json:"subject"`
}

// DropMergeError: the drop target is a merge (errors.Is ErrDropMerge).
// LeaveOut is what leaving the whole merge out would lose.
type DropMergeError struct {
	Commit   string
	LeaveOut []LostCommit
}

func (e *DropMergeError) Error() string {
	return fmt.Sprintf("%s: %v", e.Commit, ErrDropMerge)
}

func (e *DropMergeError) Is(target error) bool { return target == ErrDropMerge }

// DropForeignError: dropping the commit would replay ANOTHER agent's commit
// outside the merge that brought it (the merge carries the dropped commit),
// re-signing it with this instance's key, which the fleet's judge refuses.
// Returned only when the caller asked to keep signatures intact (ownOnly).
// LeaveOut is what the confirmed fallback, leaving every merge that carries
// the dropped commit out whole, would lose.
type DropForeignError struct {
	Commit, Agent, Merge string
	LeaveOut             []LostCommit
}

func (e *DropForeignError) Error() string {
	return fmt.Sprintf("dropping it would re-sign agent %q's commit %s, which came in with merge %s", e.Agent, e.Commit, e.Merge)
}

// LeaveOutChangedError: what the confirmed fallback would leave out is not
// the list the operator confirmed. Nothing changed; LeaveOut is the list now.
type LeaveOutChangedError struct {
	LeaveOut []LostCommit
}

func (e *LeaveOutChangedError) Error() string {
	return "what leaving the merge out would lose has changed since it was confirmed; nothing was changed"
}

// sameCommits: the commit hashes of lost are exactly confirmed, as sets.
func sameCommits(lost []LostCommit, confirmed []string) bool {
	want := map[string]bool{}
	for _, h := range confirmed {
		want[h] = true
	}
	if len(want) != len(lost) {
		return false
	}
	for _, l := range lost {
		if !want[l.Commit] {
			return false
		}
	}
	return true
}

type dropOutcome struct {
	replayed, unfolded int
	leftOut            []LostCommit
}

// dropStep is one replay of a drop's rebuild.
type dropStep struct {
	c       *object.Commit
	asMerge bool
}

func lostCommit(c *object.Commit) LostCommit {
	subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
	return LostCommit{Commit: c.Hash.String(), Agent: agentIDOfAuthor(c.Author.Email), Author: c.Author.Name, Subject: subject}
}

// dropPlan walks branch's first-parent line from tip down to the upstream
// and answers, in replay order, what a drop of drop replays. leaveOut: the
// confirmed fallback (see DropUnpublishedCommitLeavingOutMerges), which also
// answers what is left out. Without it a merge target is a *DropMergeError
// and, with ownOnly, a foreign side commit a *DropForeignError, each
// carrying the fallback's left-out list.
func (rh *repoHandler) dropPlan(branch string, tip plumbing.Hash, onUp map[plumbing.Hash]bool, drop plumbing.Hash, ownOnly, leaveOut bool) (line []dropStep, unfolded int, leftOut []LostCommit, err error) {
	dropC, err := object.GetCommit(rh.gits, drop)
	if err != nil {
		return nil, 0, nil, err
	}
	fallback := func() []LostCommit {
		_, _, lost, ferr := rh.dropPlan(branch, tip, onUp, drop, false, true)
		if ferr != nil {
			return nil
		}
		return lost
	}
	if dropC.NumParents() > 1 && !leaveOut {
		return nil, 0, nil, &DropMergeError{Commit: drop.String(), LeaveOut: fallback()}
	}
	own, _ := rh.ownAgentID(branch)
	// groups in tip -> base order; each group's steps in replay order.
	var groups [][]dropStep
	cur, err := object.GetCommit(rh.gits, tip)
	if err != nil {
		return nil, 0, nil, err
	}
	reaches := func(from []plumbing.Hash, target plumbing.Hash) (bool, error) {
		found := false
		for _, p := range from {
			if err := walkHistory(rh.gits, p, onUp, func(c *object.Commit) {
				if c.Hash == target {
					found = true
				}
			}); err != nil {
				return false, err
			}
		}
		return found, nil
	}
	// sideOf walks a merge's other parents parents-first (walkHistory is
	// post-order), stopping at what its first parent already reaches.
	sideOf := func(m *object.Commit, visit func(c *object.Commit)) error {
		first := map[plumbing.Hash]bool{}
		for h := range onUp {
			first[h] = true
		}
		if err := walkHistory(rh.gits, m.ParentHashes[0], onUp, func(c *object.Commit) { first[c.Hash] = true }); err != nil {
			return err
		}
		for _, p := range m.ParentHashes[1:] {
			if err := walkHistory(rh.gits, p, first, func(c *object.Commit) {
				first[c.Hash] = true // a commit two parents share once
				visit(c)
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for !onUp[cur.Hash] {
		carries := false
		if cur.NumParents() > 1 {
			if carries, err = reaches(cur.ParentHashes[1:], drop); err != nil {
				return nil, 0, nil, err
			}
		}
		switch {
		case leaveOut && (cur.Hash == drop || carries):
			// The fallback: the merge (or drop itself) is left out whole,
			// with everything its side brought.
			leftOut = append(leftOut, lostCommit(cur))
			if cur.NumParents() > 1 {
				if err := sideOf(cur, func(c *object.Commit) { leftOut = append(leftOut, lostCommit(c)) }); err != nil {
					return nil, 0, nil, err
				}
			}
		case cur.NumParents() > 1 && !carries:
			// Every merge that does not carry drop is replayed as a merge of
			// the same parents (#533 review S1).
			groups = append(groups, []dropStep{{c: cur, asMerge: true}})
		case cur.NumParents() > 1:
			// A merge whose other parents reach drop (it brought drop in, or
			// its side forked after drop) cannot be replayed as a merge: its
			// history would carry drop again. Its side's OWN commits are
			// replayed one by one, parents first, drop excluded (#533
			// re-review S1), so nothing but drop's own change is lost.
			unfolded++
			var side []dropStep
			var werr error
			if err := sideOf(cur, func(c *object.Commit) {
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
				side = append(side, dropStep{c: c})
			}); err != nil {
				return nil, 0, nil, err
			}
			if werr != nil {
				werr.(*DropForeignError).LeaveOut = fallback()
				return nil, 0, nil, werr
			}
			groups = append(groups, side)
		case cur.Hash != drop:
			groups = append(groups, []dropStep{{c: cur}})
		}
		if cur.NumParents() == 0 {
			break
		}
		if cur, err = cur.Parent(0); err != nil {
			return nil, 0, nil, err
		}
	}
	for i := len(groups) - 1; i >= 0; i-- {
		line = append(line, groups[i]...)
	}
	return line, unfolded, leftOut, nil
}
