package verify

import (
	"context"
	"errors"
	"fmt"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/pki"
	"knomit/internal/store/fleetrec"
	"knomit/internal/store/internal/vcs"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/rs/zerolog"
)

// CommitRefusedByJudge is the part of judgeIncoming's refusal text that says
// the judge refused a commit ("knomit: commit <hash> refused by
// verify_signatures=enforce: <rule>: <reason>"). A pusher's recorded push
// error carries it, which is how a member's status tells the host refusing
// a commit of its branch from a failure a retry can cure.
const CommitRefusedByJudge = "refused by verify_signatures=enforce"

// CommitRefusedByValidation is the part of a refusal text that says a branch's
// net change adds or changes a fact that fails the ontology's validations
// (RuleValidation): "knomit: commit <hash> refused by the ontology's
// validations: validation: <reason>", where <hash> is the commit that WROTE
// the failing content (introducedBy), not necessarily the tip. A retry of the
// same tip cannot cure it; a later commit that fixes the fact does (the net
// change is judged).
const CommitRefusedByValidation = "refused by the ontology's validations"

// incomingValid is RuleValidation on the NET change of the branch entering
// this repository (judgeIncoming's first pass): the facts its tip holds that
// differ from upstream's tip and from one or more merge bases
// (BranchFactsValid), with the ontology at upstream's tip, in every
// verify_signatures mode and in every repository. A tip the host already
// holds is judged too (it may be the invalid intermediate of a branch admitted
// fixed). Returns the refusal text, or "".
//
// An ontology at upstream that does not parse refuses EVERY push here, in
// every mode (its rules cannot be known), while the store's own writers
// (validateWrite) skip validation on such a branch: a repository that cannot
// read its ontology is made unwritable where it is opened, so the writers
// need no second gate, but a branch entering from elsewhere cannot be judged
// at all. Before #533 an off-mode host read only verify_signatures and
// admitted such pushes; the likely trigger is an older binary hosting an
// ontology a newer one wrote (#533 review N3).
func incomingValid(ctx context.Context, rh *vcs.Repo, upstream string, tip plumbing.Hash, commits []*object.Commit) string {
	var tipC *object.Commit
	for _, c := range commits {
		if c.Hash == tip {
			tipC = c
		}
	}
	if tipC == nil {
		// A tip this host already holds: still judged (#533 re-review S3).
		// Under net-change judging the host holds the invalid intermediates
		// of branches it admitted fixed, so a ref moved back onto one of
		// them must be refused here, not only by the merge guard.
		held, err := object.GetCommit(rh.Storer(), tip)
		if err != nil {
			return ""
		}
		tipC = held
	}
	h, err := rh.ResolveRef(ctx, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: the ontology at %s could not be read: %v", upstream, err)
	}
	c, err := object.GetCommit(rh.Storer(), h)
	if err != nil {
		return fmt.Sprintf("knomit: the ontology at %s could not be read: %v", upstream, err)
	}
	ont, err := OntologyOf(c)
	if err != nil {
		return fmt.Sprintf("knomit: the ontology at %s could not be read: %v", upstream, err)
	}
	if r, ok := BranchFactsValid(tipC, c, ont, rh.OntologyRoot()); !ok {
		return fmt.Sprintf("knomit: commit %s %s: %s: %s", r.Commit, CommitRefusedByValidation, r.Rule, r.Reason)
	}
	return ""
}

// judgeIncoming is the judge where a branch ENTERS this repository, shared by
// the two entries: receive-pack (a push to a knomit host, verify) and the
// fleet master's fetch (AdmitPeerBranch, a forge). It first runs the
// ontology's validations on the branch's net change at tip (incomingValid,
// every mode), then reads verify_signatures at upstream's tip,
// loads the fleet from fleet, and judges every commit with JudgeCommit;
// signer, when not nil, is one more rule for a commit JudgeCommit accepted
// (receive-pack's signer = pusher; a fetch has no pusher). Returns the
// refusal text, or "".
func JudgeIncoming(ctx context.Context, rh *vcs.Repo, upstream string, fleetOf func(context.Context) (fleetrec.Fleet, error), signer func(*object.Commit) (Refusal, bool), tip plumbing.Hash, commits []*object.Commit, lg zerolog.Logger) string {
	if msg := incomingValid(ctx, rh, upstream, tip, commits); msg != "" {
		return msg
	}
	mode, ont, err := VerifyModeAt(ctx, rh, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: verify_signatures at %s could not be read: %v", upstream, err)
	}
	if mode == VerifyOff || len(commits) == 0 {
		return ""
	}
	var fleet fleetrec.Fleet
	if fleetOf == nil {
		err = ErrNoFleet
	} else {
		fleet, err = fleetOf(ctx)
	}
	if err != nil {
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: verify_signatures is enforce but this instance has no fleet repository to judge against: %v", err)
		}
		lg.Warn().Err(err).Msg("receive-pack: verify_signatures is log but there is no fleet repository; commits not judged")
		return ""
	}
	// Rule A (self-registration) exists only where member records live: this
	// repository must itself be the fleet repository (its ontology at the
	// upstream is the fleet ontology).
	selfRoot := ""
	if fact.IsFleetOntology(ont) {
		if selfRoot = rh.FactRoot(); selfRoot == "" {
			selfRoot = "kb"
		}
	}
	gate := NewJudge(fleet, selfRoot, ont, rh.FactRoot())
	// A member's branch, never this instance's own: the host-record rule
	// applies (hostRecord).
	gate.Entry = true
	for _, c := range commits {
		ref, ok := gate.JudgeCommit(c)
		if ok && signer != nil {
			ref, ok = signer(c)
		}
		if ok {
			continue
		}
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: commit %s %s: %s: %s", c.Hash, CommitRefusedByJudge, ref.Rule, ref.Reason)
		}
		lg.Warn().Str("commit", c.Hash.String()).Str("rule", ref.Rule).Str("reason", ref.Reason).
			Msg("receive-pack: commit would be refused (verify_signatures=log)")
	}
	return ""
}

// SignerIsPusher refuses a signed commit whose SSHSIG key is not the pusher's
// certificate key. Called only for a commit JudgeCommit accepted, so a commit
// without a verifiable signature here is a two-parent merge that passed M3
// (it adds nothing beyond its parents; unsigned, or a signature that does not
// verify) and has no signer to compare. A merge that passed M3 with a
// signature that DOES verify still needs it to be the pusher's.
//
// On such a clean merge this check protects nothing: a pusher can strip or
// corrupt the signature and the same merge is admitted (the S2 ruling in
// judgeSigner). That is harmless, because a clean merge carries no content
// beyond its parents, and its parents are either new in this push (judged
// on their own) or held commits reachable from main or an admitted agent
// branch (on the fleet repository newCommits judges any other held commit as
// if new); the check matters for every other signed commit.
//
// Only the commits newCommits returns reach this: those the push sent, and on
// the fleet repository also every held commit that is NOT reachable from main
// or from a local agent/* branch (heldAdmitted). A member merging the host's
// main into its branch pushes other members' commits, but those are on main
// or on admitted branches, so only the member's own merge commit is judged.
//
// INVARIANT this relies on: every commit the walk skips is reachable from
// main or from a ref that was itself admitted. On the fleet repository the
// walk enforces it (PR #524 B1): its copy can hold commits nobody judged (a
// master with an origin fetches refused member tips; a member clone made
// before PR #524 copied every agent/* branch), and a clean merge of such a
// commit and main would otherwise be the only commit judged, admitting the
// unjudged one through it. A fleet copy with an origin also accepts no push at
// all (repos.Manager.FleetPushRefusal, at the web edge in pushPolicy): defence
// in depth, and "nobody pushes to a non-master". Every other repository keeps
// "held = judged or written here", which holds because it never fetches
// another agent's branch after its clone.
func SignerIsPusher(c *object.Commit, pusher string) (Refusal, bool) {
	s, err := VerifyCommitSignature(c)
	if err != nil {
		return Refusal{}, true
	}
	if !strings.EqualFold(s.Fingerprint, pusher) {
		return Refusal{Commit: c.Hash.String(), Rule: RuleNotPusher, SignerFP: s.Fingerprint,
			Reason: fmt.Sprintf("signed by %s, not by the pusher %s", pki.Short(s.Fingerprint), pki.Short(pusher))}, false
	}
	return Refusal{}, true
}

// privatePathClosure is receivePack.privatePathClosure for any entry: st
// holds the commits (a push's quarantine; the store itself for a fetch).
func PrivatePathClosure(ctx context.Context, rh *vcs.Repo, st storer.EncodedObjectStorer, upstream string, commits []*object.Commit) string {
	if len(commits) == 0 {
		return ""
	}
	uh, err := rh.ResolveRef(ctx, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: %s could not be read: %v", upstream, err)
	}
	up, err := object.GetCommit(st, uh)
	if err != nil {
		return fmt.Sprintf("knomit: %s could not be read: %v", upstream, err)
	}
	shapeOK := map[plumbing.Hash]bool{}
	for _, c := range commits {
		// The closure's DiffTree ends silently at a directory entry that is
		// not a tree (as does every go-git walker; CheckTreeShape), so a
		// bogus entry would hide the private paths after it. reachableNew
		// does not catch one that names an object the host already holds.
		// Refused here, in every verify_signatures mode, with the judge's
		// rule name.
		if err := commitTreeShape(c, shapeOK); err != nil {
			return fmt.Sprintf("knomit: commit %s refused: %s: %v", c.Hash, RuleMalformedTree, err)
		}
		p, err := privateChangeFromBase(c, up)
		if err != nil {
			return fmt.Sprintf("knomit: commit %s could not be compared with %s: %v", c.Hash, upstream, err)
		}
		if p != "" {
			return fmt.Sprintf("knomit: commit %s changes %s relative to %s; paths with a segment beginning with \".\" (such as %s/) cannot be pushed", c.Hash, p, upstream, fact.PrivateRoot)
		}
	}
	return ""
}

// privateChangeFromBase returns the first private path (sorted) whose content
// in c differs from that at the merge base of c and up, or "". With several
// best bases (a criss-cross) matching ANY of them is enough. On its own that
// would be unsafe — the merge machinery picks one base, and a commit matching
// only the other could revert through it — but push time is only a fast
// fail: the authority is the merge-time guard (guardPrivate in MergePushed
// and MergeConsensus), which refuses any merge whose result changes the host
// agent branch's private paths. Requiring EVERY base instead wedged a member
// branch for good on a benign criss-cross (its merge stays new on every later
// push).
//
// c's tree must be well formed (privatePathClosure checks it first): DiffTree
// ends silently at a directory entry that is not a tree. A base tree is one
// the host holds; one truncated there only makes c's paths look added, which
// refuses rather than hides.
func privateChangeFromBase(c, up *object.Commit) (string, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	bases, err := c.MergeBase(up)
	if err != nil {
		return "", err
	}
	baseTrees := []*object.Tree{nil} // no merge base: the empty tree
	if len(bases) > 0 {
		baseTrees = baseTrees[:0]
		for _, b := range bases {
			bt, err := b.Tree()
			if err != nil {
				return "", err
			}
			baseTrees = append(baseTrees, bt)
		}
	}
	first := ""
	for i, bt := range baseTrees {
		changes, err := object.DiffTree(bt, tree)
		if err != nil {
			return "", err
		}
		out := ""
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if name != "" && fact.IsPrivatePath(name) && (out == "" || name < out) {
					out = name
				}
			}
		}
		if out == "" {
			return "", nil // matches this base
		}
		if i == 0 {
			first = out
		}
	}
	return first, nil
}

// verifyModeAt reads verify_signatures from the ontology at branch's tip, and
// that ontology: the judge reads from it whether this repository is the fleet
// (rule A) and its artifact rules (R7). With verification on, an ontology
// that does not parse is an error: its rules cannot be known.
func VerifyModeAt(ctx context.Context, rh *vcs.Repo, branch string) (mode string, ont *fact.Ontology, err error) {
	h, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", nil, err
	}
	c, err := object.GetCommit(rh.Storer(), h)
	if err != nil {
		return "", nil, err
	}
	raw, err := vcs.TreeOntology(c)
	if err != nil {
		return "", nil, err
	}
	if raw == nil {
		return VerifyOff, nil, nil
	}
	vs, err := fact.ReadVerifySettings(raw)
	if err != nil {
		return "", nil, err
	}
	if !vs.Valid {
		return "", nil, errors.New("unknown value")
	}
	if vs.Mode == VerifyOff {
		return VerifyOff, nil, nil
	}
	if ont, err = fact.ParseOntology(raw); err != nil {
		return "", nil, err
	}
	return vs.Mode, ont, nil
}
