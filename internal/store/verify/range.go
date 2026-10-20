package verify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store/fleetrec"
	"knomit/internal/store/internal/vcs"
)

// ErrNoFleet: verification is on in the knowledge base but no fleet
// repository was given to judge the range against.
var ErrNoFleet = errors.New("verification is on but no fleet repository was given")

// RangeInput names what CheckRange compares: a knowledge base's main and a
// candidate (both revisions in KBDir), and the fleet checkout that says who
// may sign.
type RangeInput struct {
	KBDir     string
	Main      string // the upstream revision in KBDir, e.g. "origin/main"
	Candidate string // the candidate revision in KBDir, e.g. an agent branch tip
	FleetDir  string // the fleet repository checkout; untouched when verification is off
	FleetRev  string // the fleet revision holding the accepted records ("" = "HEAD")
	FleetRoot string // the fleet's fact root ("" = "kb")
	// KBRoot is the judged knowledge base's fact root, where RuleValidation
	// finds its facts ("" = FleetRoot, then "kb").
	KBRoot string
}

// RangeVerdict is the acceptance gate's answer for one candidate.
type RangeVerdict struct {
	Mode    string    `json:"mode"`    // verify_signatures at Main's tip
	Checked int       `json:"checked"` // commits in the candidate range
	Refused []Refusal `json:"refused"`
}

// ExitCode is the gate's contract for a CI job: 0 mergeable (a clean range,
// or signature failures in log mode or with verification off, which are
// reported only), 1 blocked (failures in enforce, or ANY RuleValidation or
// RuleMergeConflict refusal, which no mode waives). "Could not run" (2) is an error from
// CheckRange, never a verdict.
func (v RangeVerdict) ExitCode() int {
	for _, r := range v.Refused {
		if v.Mode == VerifyEnforce || r.Rule == RuleValidation || r.Rule == RuleMergeConflict {
			return 1
		}
	}
	return 0
}

// CheckRange is F09's acceptance gate: it runs ONCE, where a change enters a
// knowledge base's main (the merge job, via `knomit verify ci`), over plain git
// checkouts so it needs no knomit instance.
//
//  0. The candidate's NET change is checked against the ontology's
//     validations at Main's tip (RuleValidation: each fact the candidate's
//     tip holds that differs from Main and from every merge base), and so is
//     what merging it into Main lands (mergeResultValid; a merge that is not
//     clean is RuleMergeConflict), in EVERY mode, the fleet untouched; a
//     refusal blocks.
//  1. verify_signatures is read at Main's tip. Off or absent: the verdict is
//     off and nothing else runs; the fleet is never touched.
//  2. The fleet's member records and certificate artifacts (the files its
//     ontology declares pki: root, crl and leaf) are loaded from the ONE fleet
//     revision (malformed records skipped and logged). A missing or unreadable
//     fleet, or one whose ontology is not the fleet ontology, is an error: the
//     gate could not run. Missing artifacts are not an error: no PKI.
//  3. Every commit in Candidate ∖ Main is judged by JudgeCommit.
func CheckRange(in RangeInput) (RangeVerdict, error) {
	kb, err := fleetrec.OpenGitDir(in.KBDir)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	mainC, err := fleetrec.ResolveCommit(kb, in.Main)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	raw, err := vcs.TreeOntology(mainC)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: ontology at %s: %w", in.Main, err)
	}
	settings := fact.VerifySettings{Mode: VerifyOff, Valid: true}
	if raw != nil {
		if settings, err = fact.ReadVerifySettings(raw); err != nil {
			return RangeVerdict{}, fmt.Errorf("verify: %w", err)
		}
	}
	if !settings.Valid {
		return RangeVerdict{}, fmt.Errorf("verify: verify_signatures at %s has an unknown value", in.Main)
	}
	out := RangeVerdict{Mode: settings.Mode, Refused: []Refusal{}}
	// The ontology at main's tip says which validations hold (RuleValidation,
	// every mode) and, with verification on, the artifact rules (R7). One
	// that does not parse cannot say which rules hold: the gate could not
	// run, never a pass.
	var ont *fact.Ontology
	if raw != nil {
		if ont, err = fact.ParseOntology(raw); err != nil {
			return RangeVerdict{}, fmt.Errorf("verify: ontology at %s: %w", in.Main, err)
		}
	}
	candC, err := fleetrec.ResolveCommit(kb, in.Candidate)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(kb.Storer, mainC.Hash, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	var commits []*object.Commit
	if err := vcs.WalkHistory(kb.Storer, candC.Hash, onMain, func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	// RuleValidation first, in EVERY mode and every repository, on the
	// candidate's NET change (BranchFactsValid): a fact it adds or changes
	// against main and one or more of their merge bases must keep the ontology's validations,
	// as the MCP write path requires. The refusal names the commit that wrote the
	// failing content (introducedBy).
	kbRoot := in.KBRoot
	if kbRoot == "" {
		kbRoot = in.FleetRoot
	}
	if len(commits) > 0 {
		if r, ok := BranchFactsValid(candC, mainC, ont, kbRoot); !ok {
			out.Refused = append(out.Refused, r)
		} else if r, ok, err := mergeResultValid(in.KBDir, kb, mainC, candC, ont, kbRoot); err != nil {
			return RangeVerdict{}, fmt.Errorf("verify: %w", err)
		} else if !ok {
			out.Refused = append(out.Refused, r)
		}
	}
	if settings.Mode == VerifyOff {
		// Checked counts the commits the signature rules judged: none.
		return out, nil
	}

	if in.FleetDir == "" {
		return RangeVerdict{}, ErrNoFleet
	}
	rev := in.FleetRev
	if rev == "" {
		rev = "HEAD"
	}
	fleet, err := fleetrec.LoadFleetAt(in.FleetDir, rev, in.FleetRoot)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	// Rule A (self-registration) exists only where member records live: the
	// knowledge base being judged must itself be the fleet repository.
	selfRoot := ""
	if fact.IsFleetOntology(ont) {
		if selfRoot = in.FleetRoot; selfRoot == "" {
			selfRoot = "kb"
		}
	}
	gate := NewJudge(fleet, selfRoot, ont, in.FleetRoot)
	for _, c := range commits {
		out.Checked++
		if r, ok := gate.JudgeCommit(c); !ok {
			out.Refused = append(out.Refused, r)
		}
	}
	return out, nil
}

// mergeResultValid judges what merging the candidate into main LANDS, which
// is not the candidate's tip when main has moved since the fork (#533 review
// 3): git merges a fact hunk by hunk, so two sides that each keep the
// ontology's validations can combine into a fact that breaks a cross-field
// rule (main shortens the title, the branch raises the confidence). The
// merge is git's own, `git merge-tree --write-tree` (the ort merge `gh pr
// merge` makes; go-git v5 has no content merge), run in kbDir; it writes the
// result's objects there and touches no ref, index or work tree. Every fact
// in the result that differs from main is validated against main's
// ontology. A merge that does not come out clean is refused
// (RuleMergeConflict): the gate cannot judge what a hand resolution would
// land. Nothing to do when main is an ancestor of the candidate: the merge
// is the candidate's tip, which BranchFactsValid judged. Needs git 2.38 or
// later; a git that cannot run the merge is an error (exit 2), never a pass.
//
// The merge reads its attributes (merge drivers such as merge=union) from
// MAIN's tree (`-c attr.tree=<main>`, git 2.42 or later), never from the
// checkout's work tree, which CI checks out at the candidate: a candidate's
// .gitattributes must not choose how its own merge is judged (#533 review 4
// NIT-B). A git from 2.38 to 2.41 ignores the key and reads the work tree's
// attributes.
func mergeResultValid(kbDir string, kb *gogit.Repository, mainC, candC *object.Commit, ont *fact.Ontology, root string) (Refusal, bool, error) {
	if ff, err := mainC.IsAncestor(candC); err != nil {
		return Refusal{}, false, fmt.Errorf("is main an ancestor of the candidate: %w", err)
	} else if ff {
		return Refusal{}, true, nil
	}
	cmd := exec.Command("git", "-C", kbDir, "-c", "attr.tree="+mainC.Hash.String(),
		"merge-tree", "--write-tree", "--no-messages", mainC.Hash.String(), candC.Hash.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	clean := err == nil
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return Refusal{}, false, fmt.Errorf("git merge-tree (git 2.38 or later): %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	refused := func(rule, reason string) (Refusal, bool, error) {
		return Refusal{Commit: candC.Hash.String(), Rule: rule, Reason: reason}, false, nil
	}
	if !clean {
		return refused(RuleMergeConflict, "the branch does not merge cleanly into main, so what a merge would land cannot be judged; bring main into the branch and resolve it there")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	treeC, err := object.GetTree(kb.Storer, plumbing.NewHash(line))
	if err != nil {
		return Refusal{}, false, fmt.Errorf("the merge result %q: %w", line, err)
	}
	mainTree, err := mainC.Tree()
	if err != nil {
		return Refusal{}, false, fmt.Errorf("main's tree: %w", err)
	}
	if root == "" {
		root = vcs.DefaultOntologyRoot
	}
	paths, err := newFactPaths(root, []*object.Tree{mainTree}, treeC)
	if err != nil {
		return Refusal{}, false, fmt.Errorf("the merge result's facts: %w", err)
	}
	_, reason, err := invalidAmong(ont, root, paths, treeC)
	if err != nil {
		return Refusal{}, false, fmt.Errorf("the merge result's facts: %w", err)
	}
	if reason != "" {
		return refused(RuleValidation, "merging the branch into main would land a fact that the branch and main each keep valid apart: "+reason+"; bring main into the branch and fix it there")
	}
	return Refusal{}, true, nil
}

// memberIndex is the fleet's member records, by agent id and by current key.
type memberIndex struct {
	byAgent map[string][]fleetrec.Member
	byKey   map[string][]fleetrec.Member // wire key -> records whose CURRENT key it is
}

func indexMembers(ms []fleetrec.Member) memberIndex {
	idx := memberIndex{byAgent: map[string][]fleetrec.Member{}, byKey: map[string][]fleetrec.Member{}}
	for _, m := range ms {
		idx.byAgent[m.Agent] = append(idx.byAgent[m.Agent], m)
		k := string(m.Key.Marshal())
		idx.byKey[k] = append(idx.byKey[k], m)
	}
	return idx
}

// Judge is everything JudgeCommit reads: the fleet at ONE revision (member
// records, certificate artifacts), where the judged repository's member
// records live, and the judged repository's ontology at main (its artifact
// rules).
type Judge struct {
	memberIndex
	pki *fleetrec.PKI // nil: the fleet revision declares or holds no root
	// membersDir is "<root>/members/" of the JUDGED repository when that
	// repository is the fleet repository itself, else "" (rule A off: there
	// is nothing to self-register into).
	membersDir string
	// ont is the JUDGED repository's ontology at main, read at the same
	// revision as verify_signatures; its artifacts section is enforced on
	// every commit (artifactRules). nil: no rules.
	ont *fact.Ontology
	// kbRoot is the judged repository's fact root, where leaf-renewal reads
	// the member records of the commit's own tree.
	kbRoot string
	// shapeOK memoises the trees CheckTreeShape found well formed, across
	// the commits this judge judges (they share most subtrees).
	shapeOK map[plumbing.Hash]bool
	// Entry says the commits are a MEMBER's branch entering this repository
	// (judgeIncoming: a push to a knomit host, the fleet master's judge of a
	// fetched or local agent/* branch), never this instance's own: the
	// host-record rule (hostRecord) runs only there. CheckRange judges any
	// range, the master's own commits included, so it is false there.
	Entry bool
	// OwnOntology lets the record-owner rule admit a change of the
	// repository's ontology file (fact.OntologyPathsNewestFirst). Only
	// judgeOwnRange sets it, for this instance's OWN commits: the fleet
	// ontology is written by the instance that creates the fleet and
	// refreshed by its master (refreshFleetOntology), never by a member
	// (a pushed branch never changes a private path: privatePathClosure,
	// guardPrivate).
	OwnOntology bool
}

// isOntologyPath reports whether p is one of the ontology file's paths.
func isOntologyPath(p string) bool {
	for _, o := range fact.OntologyPathsNewestFirst() {
		if p == o {
			return true
		}
	}
	return false
}

// NewJudge builds the judge for one fleet revision. selfRoot is the judged
// repository's fact root when that repository is the fleet repository, and
// "" when it is not (rule A off). ont is the judged repository's ontology at
// main and kbRoot its fact root ("" = "kb").
func NewJudge(f fleetrec.Fleet, selfRoot string, ont *fact.Ontology, kbRoot string) Judge {
	if kbRoot == "" {
		kbRoot = "kb"
	}
	j := Judge{memberIndex: indexMembers(f.Members), pki: f.PKI, ont: ont, kbRoot: kbRoot, shapeOK: map[plumbing.Hash]bool{}}
	if selfRoot != "" {
		j.membersDir = path.Join(selfRoot, fact.MembersTopic) + "/"
	}
	return j
}

// mayRegister is R4's gate on rule A: the id has NO active record, and the
// signing key is not the current key of a record for a DIFFERENT id (in any
// state). Rule A is a join or a rejoin; it never takes over an id an active
// record holds, and it never makes one key the current key of two ids.
//
// The id's OWN inactive record holding the signing key does not block: that
// is a same-key rejoin. Leave = revoke, so the leaf that held the key is on
// the CRL, and rule A admits the rejoin only on a fresh leaf the master
// issued after the leave (selfRegistration) — that leaf is the approval.
// MISREADING: this is not "any record's key" (that refuses every same-key
// rejoin), and not "an active record's key" (that lets two ids share a key).
func (j Judge) mayRegister(id string, s CommitSigner) bool {
	for _, m := range j.byAgent[id] {
		if m.State == fact.MemberActive {
			return false
		}
	}
	for _, m := range j.byKey[string(s.Key.Marshal())] {
		if m.Agent != id {
			return false
		}
	}
	return true
}

// JudgeCommit applies the gate's rules to one commit:
//   - FIRST, in every repository, the commit's tree and every parent's tree
//     must be well formed (commitTreeShape, else RuleMalformedTree): every
//     rule below walks those trees with go-git walkers that end silently at
//     a directory entry that is not a tree, and would judge only a prefix;
//   - a two-parent merge that adds nothing beyond its parents (M3) passes,
//     signed or not, BEFORE any record rule (H4): a member's routine "merge
//     main into my branch" is never refused as no-record or not-owner, even
//     though against main's side it brings in other members' changes. A merge
//     that fails M3 is judged like any commit when signed, and refused
//     (RuleMerge) when unsigned;
//   - otherwise the SSHSIG must verify;
//   - the author address's agent id must have exactly ONE member record;
//   - the signing key (the full key) must be that record's CURRENT key, and no
//     other record's;
//   - the record's state must be active;
//   - rule B (only when the fleet revision holds root.crt): the leaf artifact
//     for the id must name that key, chain to the root and not be on the CRL
//     (certified: RuleNoCert, RuleKeyMismatch, RuleRevoked);
//   - the host-record rule (fleet repository, a member's branch entering it
//     only: hostRecord), before the record rules: a signed commit that
//     changes artifacts/fleet/host is refused, since only the master writes
//     it and its commits never enter through a member's branch;
//   - the record-owner rule (fleet repository only, recordOwner): every path
//     the commit changes outside artifacts/, against every parent, is under
//     members/<author id>/, and none of them is a delete;
//   - rule A: a commit refused above for having no record, a record for
//     another key, or an inactive record is ADMITTED when it is a
//     self-registration backed by a valid leaf artifact (selfRegistration) —
//     only when the id has no active record and the signing key is not the
//     current key of another id's record (R4, mayRegister), and only when it
//     keeps the record-owner rule;
//   - the artifact rules (R7): every commit the rules above admit, signed or
//     an M3 merge, must also keep every artifact rule of the judged
//     repository's ontology at main (artifactRules).
//
// Nothing is derived from the id's text (D18): it is only the key under which
// a record or a leaf file is looked up, and compared for equality with the
// leaf's email SAN id.
func (j Judge) JudgeCommit(c *object.Commit) (Refusal, bool) {
	if err := commitTreeShape(c, j.shapeOK); err != nil {
		return Refusal{Commit: c.Hash.String(), Rule: RuleMalformedTree, Reason: err.Error()}, false
	}
	r, ok := j.judgeSigner(c)
	if !ok {
		return r, false
	}
	return j.artifactRules(c)
}

// judgeSigner is every rule of JudgeCommit but the artifact rules.
func (j Judge) judgeSigner(c *object.Commit) (Refusal, bool) {
	h := c.Hash.String()
	s, serr := VerifyCommitSignature(c)
	unsigned := serr != nil && (errors.Is(serr, ErrUnsigned) || errors.Is(serr, ErrNotSSHSIG))
	if c.NumParents() >= 2 {
		v, err := CheckM3(c)
		// DECISION (user ruling, PR #523 S2): a clean merge is admitted
		// whatever its signature state: unsigned, a signature that verifies,
		// or one that is present and does NOT verify. Its signature is not
		// verified here because the merge carries no content beyond its
		// parents, and stripping the signature would give the same verdict.
		// Pinned by TestM3_CleanMergeWithABadSignatureIsAdmitted and
		// TestReceivePack_CleanMergeSignatureStates.
		if err == nil && v.OK {
			return Refusal{}, true
		}
		if unsigned {
			if err != nil {
				return Refusal{Commit: h, Rule: RuleMerge, Reason: err.Error()}, false
			}
			return Refusal{Commit: h, Rule: RuleMerge, Reason: v.Reason}, false
		}
	}
	if serr != nil {
		return Refusal{Commit: h, Rule: RuleSignature, Reason: serr.Error()}, false
	}
	fp := s.Fingerprint
	id := vcs.AgentIDOfAuthor(c.Author.Email)
	if id == "" {
		return Refusal{Commit: h, Rule: RuleNoRecord, SignerFP: fp, Reason: fmt.Sprintf("author %q is not an agent address", c.Author.Email)}, false
	}
	if r, ok := j.hostRecord(c, id); !ok {
		return r, false
	}
	r, ok := j.judgeRecord(h, id, s)
	switch {
	case ok:
		if r, ok := j.certified(h, id, s); !ok {
			return r, false
		}
		return j.recordOwner(c, id)
	case (r.Rule == RuleNoRecord || r.Rule == RuleKeyMismatch || r.Rule == RuleInactive) && j.mayRegister(id, s):
		return j.selfRegistration(c, id, s, r)
	}
	return r, false
}

// hostRecord is the host-record rule, on the fleet repository and for a
// member's branch entering it only (j.Entry: receive-pack and the master's
// peer judge, never CheckRange). Like the record-owner rule it is baked, not
// declared: no commit of a member's branch may change artifacts/fleet/host
// (fact.FleetHostPath) or artifacts/fleet/master (fact.FleetMasterPath, the
// acting master's marker), in any case spelling (the artifact patterns match
// case-insensitively), against ANY parent. The host record says where the
// fleet is hosted, and only the fleet's master writes it, at create and when
// it becomes the host; the master's own commits never enter through a member
// branch (the hub moves main onto its own agent branch, a forge-hosted master
// publishes it), so a member that changed it could only be redirecting every
// member's fleet URL. The path is a DECLARED artifact, which the record-owner
// rule exempts, so this rule, not that one, is what refuses it. It runs
// before the record rules and rule A, for every signed commit that is not a
// clean merge: a member's merge of main that brings in the master's change
// (M3) adds nothing and is admitted before it.
// MISREADING: CheckRange (`knomit verify ci`) does not run it, so the
// master's create commits, the host record included, pass that gate.
func (j Judge) hostRecord(c *object.Commit, id string) (Refusal, bool) {
	if j.membersDir == "" || !j.Entry {
		return Refusal{}, true
	}
	refuse := func(reason string) (Refusal, bool) {
		return Refusal{Commit: c.Hash.String(), Rule: RuleHostRecord, Reason: reason}, false
	}
	tree, err := c.Tree()
	if err != nil {
		return refuse(fmt.Sprintf("the commit's tree cannot be read: %v", err))
	}
	parents, err := vcs.ParentTrees(c)
	if err != nil {
		return refuse(fmt.Sprintf("a parent's tree cannot be read: %v", err))
	}
	for _, pt := range parents {
		changes, err := object.DiffTree(pt, tree)
		if err != nil {
			return refuse(fmt.Sprintf("diff: %v", err))
		}
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if strings.EqualFold(name, fact.FleetHostPath) {
					return refuse(fmt.Sprintf("agent %q changes %s: only the fleet's master writes where the fleet is hosted", id, name))
				}
				if strings.EqualFold(name, fact.FleetMasterPath) {
					return refuse(fmt.Sprintf("agent %q changes %s: only the fleet's master writes who its acting master is", id, name))
				}
			}
		}
	}
	return Refusal{}, true
}

// judgeRecord is the record rules: exactly one record at id, whose current key
// is the signing key and no other record's, in state active.
func (j Judge) judgeRecord(h, id string, s CommitSigner) (Refusal, bool) {
	fp := s.Fingerprint
	recs := j.byAgent[id]
	switch len(recs) {
	case 0:
		return Refusal{Commit: h, Rule: RuleNoRecord, SignerFP: fp, Reason: fmt.Sprintf("no member record for agent %q", id)}, false
	case 1:
	default:
		return Refusal{Commit: h, Rule: RuleAmbiguous, SignerFP: fp, Reason: fmt.Sprintf("%d member records for agent %q", len(recs), id)}, false
	}
	rec := recs[0]
	if !fact.SameKey(rec.Key, s.Key) {
		return Refusal{Commit: h, Rule: RuleKeyMismatch, SignerFP: fp, Reason: fmt.Sprintf("signing key is not the current key of %q", id)}, false
	}
	if n := len(j.byKey[string(s.Key.Marshal())]); n > 1 {
		return Refusal{Commit: h, Rule: RuleDuplicateKey, SignerFP: fp, Reason: fmt.Sprintf("the signing key is the current key of %d records", n)}, false
	}
	if rec.State != fact.MemberActive {
		return Refusal{Commit: h, Rule: RuleInactive, SignerFP: fp, Reason: fmt.Sprintf("agent %q is %s", id, rec.State)}, false
	}
	return Refusal{}, true
}

// certified is rule B, for a commit the record rules accepted (so the
// record's key IS the signing key). With no PKI at the fleet revision it
// accepts: the rules are exactly what they were before the artifacts existed.
// With PKI it FAILS CLOSED: the commit is accepted only when the leaf
// artifact for id names the record's key, chains to the root, and is not on
// a CRL the root signed. Otherwise:
//   - no leaf, a leaf the root did not sign, or a root or CRL nobody can
//     read: RuleNoCert;
//   - a leaf for another key: RuleKeyMismatch;
//   - a listed serial: RuleRevoked.
//
// Failing open here would let a record keep signing after its key's leaf
// was revoked, simply because the leaf at certs/<id>.pem names some other
// key (or is gone).
func (j Judge) certified(h, id string, s CommitSigner) (Refusal, bool) {
	if j.pki == nil {
		return Refusal{}, true
	}
	leaf, err := j.leafFor(id, s)
	r := Refusal{Commit: h, SignerFP: s.Fingerprint}
	switch {
	case err == nil && leaf != nil:
		return Refusal{}, true
	case err == nil:
		r.Rule, r.Reason = RuleNoCert, fmt.Sprintf("the fleet has certificates but no %s at the fleet revision", j.pki.Layout.LeafPath(id))
	case errors.Is(err, errLeafOtherKey):
		r.Rule, r.Reason = RuleKeyMismatch, fmt.Sprintf("%s is for another key than the member record's", j.pki.Layout.LeafPath(id))
	case errors.Is(err, pki.ErrRevoked):
		r.Rule, r.Reason = RuleRevoked, fmt.Sprintf("the certificate for agent %q (serial %s) is on the fleet CRL", id, leaf.SerialNumber.Text(16))
	default:
		r.Rule, r.Reason = RuleNoCert, fmt.Sprintf("the certificate for agent %q does not hold: %v", id, err)
	}
	return r, false
}

// selfRegistration is rule A. The commit was refused by the record rules
// with refusal; it is ADMITTED instead when:
//   - it keeps the record-owner rule (recordOwner: every path it changes
//     outside artifacts/, against EVERY parent, is under the judged fleet
//     repository's members/<id>/ and none is a delete — the author writing
//     its own record and nothing else; artifact paths are the artifact
//     rules'), and
//   - the leaf artifact certs/<id>.pem at the fleet revision exists, its key
//     equals the SIGNING key (full key), it chains to root.crt and its serial
//     is not on crl.pem, and
//   - the folder it leaves holds exactly ONE member record, for agent id, in
//     state active, whose key is that same key (selfRecord). The leaf vouches
//     for one key; without this a holder could register ANOTHER key K2 under
//     its id and sign with K2 from then on, out of reach of a revocation of
//     the leaf's key.
//
// Only "active" may be written this way: rule A runs only for an id with no
// active record for the signing key, so the one thing it can mean is a join
// or a rejoin. Leaving is a change an active member makes to its own record,
// under the ordinary rules.
//
// A commit of any other shape keeps refusal unchanged. A self-registration
// with no leaf, one that does not chain, or one whose record does not match
// keeps refusal's rule with the reason; a leaf for another key is
// RuleKeyMismatch; a revoked leaf is RuleRevoked.
func (j Judge) selfRegistration(c *object.Commit, id string, s CommitSigner, refusal Refusal) (Refusal, bool) {
	dir := j.membersDir + id + "/"
	if j.pki == nil || j.membersDir == "" {
		return refusal, false
	}
	if _, ok := j.recordOwner(c, id); !ok {
		return refusal, false
	}
	leaf, err := j.leafFor(id, s)
	if err == nil && leaf != nil {
		err = selfRecord(c, dir, id, s)
	}
	switch {
	case leaf == nil && err == nil:
		refusal.Reason += fmt.Sprintf("; self-registration not admitted: no %s at the fleet revision", j.pki.Layout.LeafPath(id))
	case errors.Is(err, errLeafOtherKey):
		refusal.Rule = RuleKeyMismatch
		refusal.Reason = fmt.Sprintf("self-registration not admitted: %s is for another key than the signing key", j.pki.Layout.LeafPath(id))
	case errors.Is(err, pki.ErrRevoked):
		refusal.Rule = RuleRevoked
		refusal.Reason = fmt.Sprintf("self-registration not admitted: the certificate for agent %q (serial %s) is on the fleet CRL", id, leaf.SerialNumber.Text(16))
	case err != nil:
		refusal.Reason += fmt.Sprintf("; self-registration not admitted: %v", err)
	default:
		return Refusal{}, true
	}
	return refusal, false
}

// selfRecord checks what a self-registration leaves in its folder dir
// (ending in "/", compared exactly) in c's tree: exactly one file, a member record
// that parses, for agent id, in state active, holding the signing key.
func selfRecord(c *object.Commit, dir, id string, s CommitSigner) error {
	tree, err := c.Tree()
	if err != nil {
		return err
	}
	var recs []fact.Member
	files := 0
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, dir) {
			return nil
		}
		files++
		content, err := f.Contents()
		if err != nil {
			return err
		}
		ff, err := fact.ParseFact(f.Name, content)
		if err != nil {
			return fmt.Errorf("the record %s does not parse: %w", f.Name, err)
		}
		m, err := fact.ParseMember(ff.Body)
		if err != nil {
			return fmt.Errorf("the record %s does not parse: %w", f.Name, err)
		}
		recs = append(recs, m)
		return nil
	})
	switch {
	case err != nil:
		return err
	case files != 1:
		return fmt.Errorf("%s holds %d files, want exactly one member record", dir, files)
	case recs[0].Agent != id:
		return fmt.Errorf("the record names agent %q, not the author %q", recs[0].Agent, id)
	case recs[0].State != fact.MemberActive:
		return fmt.Errorf("the record's state is %q; a self-registration may only write %q", recs[0].State, fact.MemberActive)
	case !fact.SameKey(recs[0].Key, s.Key):
		return errors.New("the record's key is not the signing key the certificate names")
	}
	return nil
}

// errLeafOtherKey: the leaf artifact for the id names another key than the
// signing key.
var errLeafOtherKey = errors.New("the certificate is for another key")

// leafFor reads the leaf artifact for id and verifies it for the signing key.
// (nil, nil): no leaf. (leaf, nil): the leaf names the signing key, chains to
// the root and is not revoked. Otherwise the leaf (when it parsed) and why it
// does not hold: errLeafOtherKey, pki.ErrRevoked (it chains and a CRL the
// root signed lists it), or any other chain or CRL failure.
//
// Expiry is NOT judged (designer, Q2): the judge is historical — the same
// commit must get the same verdict whenever the gate runs, in CI or on
// replay — and TLS already refuses an expired certificate on every
// connection. The chain is therefore verified at a moment inside the leaf's
// own validity, never at the current time.
func (j Judge) leafFor(id string, s CommitSigner) (*x509.Certificate, error) {
	if j.pki == nil {
		return nil, nil
	}
	leaf, err := j.pki.Cert(id)
	if leaf == nil || err != nil {
		return nil, err
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return leaf, fmt.Errorf("%w: %T", pki.ErrNotEd25519, leaf.PublicKey)
	}
	certKey, err := ssh.NewPublicKey(pub)
	if err != nil || !fact.SameKey(certKey, s.Key) {
		return leaf, errLeafOtherKey
	}
	ident, err := j.pki.VerifyLeaf(leaf)
	if err != nil {
		return leaf, err
	}
	// R2: the leaf's email SAN id must EQUAL the id it is filed under (and
	// the author's): a leaf the root issued to another agent, planted at this
	// id's path, vouches for nothing here. Equality only; nothing is parsed.
	if ident.AgentID != id {
		return leaf, fmt.Errorf("%w: its email SAN %q names agent %q, not %q", errLeafOtherID, ident.Email, ident.AgentID, id)
	}
	return leaf, nil
}

// errLeafOtherID: the leaf artifact at an id's path was issued to another
// agent id (or is an operator certificate).
var errLeafOtherID = errors.New("the certificate is not issued to this agent id")

// recordOwner is the record-owner rule, the fleet repository's only. It is
// baked, not declared: the fleet ontology is product-owned, so there is no
// attribute to switch it. A member writes its own record and nothing else:
//   - every path c changes against ANY parent (the union of the per-parent
//     diffs; the empty tree for a root commit) must be under the judged
//     repository's members/<id>/ (ending in "/", compared exactly), unless it
//     is a DECLARED artifact path (one an artifacts: pattern of the judged
//     repository's ontology at main matches). Declared paths are the artifact
//     rules' (artifactRules), which every admitted commit keeps too; so the
//     master's enroll and revoke commits, which change declared artifacts
//     only, pass. An undeclared path under artifacts/ has no artifact rule,
//     so it is the owner rule's like any other path: a member may not add,
//     modify or delete it;
//   - none of the paths under members/<id>/ may be a delete: a member leaves
//     by writing state left, never by removing its record;
//   - a member record (.md) c adds or modifies under members/<id>/ that
//     parses must be FOR id (its agent: equals id). Otherwise any member
//     could plant a record for another agent in its own folder and make that
//     agent ambiguous, or pre-empt a joiner's rule A. The loader also skips
//     such a record (fleetrec.LoadMembersOfTree), so one already on main locks no one
//     out; this refusal keeps new ones off main. A file that does not parse
//     as a record is no record: the loader skips it too.
//
// The union, not the intersection (paths that differ from EVERY parent), is
// deliberate: the intersection would admit a merge that takes the OLD side of
// another member's record, which reverts that record once merged into main
// (the reasoning of privatePathClosure). The cost: a member's merge that
// settles a conflict with main is refused not-owner, because against main's
// side it changes other members' paths. Outside the fleet repository
// (membersDir "") the rule does not exist.
//
// A two-parent merge M3 admits never reaches this: against its main-side
// parent it brings in other members' changes, yet adds nothing. A tree that
// cannot be read is a refusal.
func (j Judge) recordOwner(c *object.Commit, id string) (Refusal, bool) {
	if j.membersDir == "" {
		return Refusal{}, true
	}
	dir := j.membersDir + id + "/"
	refuse := func(reason string) (Refusal, bool) {
		return Refusal{Commit: c.Hash.String(), Rule: RuleNotOwner, Reason: reason}, false
	}
	tree, err := c.Tree()
	if err != nil {
		return refuse(fmt.Sprintf("the commit's tree cannot be read: %v", err))
	}
	parents, err := vcs.ParentTrees(c)
	if err != nil {
		return refuse(fmt.Sprintf("a parent's tree cannot be read: %v", err))
	}
	for _, pt := range parents {
		changes, err := object.DiffTree(pt, tree)
		if err != nil {
			return refuse(fmt.Sprintf("diff: %v", err))
		}
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if name != "" && !strings.HasPrefix(name, dir) && !j.declaredArtifact(name) && !(j.OwnOntology && isOntologyPath(name)) {
					return refuse(fmt.Sprintf("agent %q changes %s: a member writes only under %s (and declared artifact paths)", id, name, dir))
				}
			}
			if ch.To.Name == "" && strings.HasPrefix(ch.From.Name, dir) {
				return refuse(fmt.Sprintf("agent %q deletes %s: a member record is never deleted", id, ch.From.Name))
			}
			if ch.To.Name != "" && strings.HasPrefix(ch.To.Name, dir) && strings.HasSuffix(ch.To.Name, ".md") {
				agent, err := recordAgent(ch)
				if err != nil {
					return refuse(fmt.Sprintf("%s cannot be read: %v", ch.To.Name, err))
				}
				if agent != "" && agent != id {
					return refuse(fmt.Sprintf("agent %q writes %s, a record for agent %q: a member writes only its own record", id, ch.To.Name, agent))
				}
			}
		}
	}
	return Refusal{}, true
}

// declaredArtifact reports whether repoPath is a declared artifact path: under
// artifacts/ and matched by a pattern of the judged repository's ontology. A
// path whose rules are ambiguous counts as declared: artifactRules refuses it
// (RuleArtifactRules) with the reason.
func (j Judge) declaredArtifact(repoPath string) bool {
	if !underArtifacts(repoPath) {
		return false
	}
	attrs, err := j.ont.ArtifactAttrs(repoPath)
	return err != nil || attrs.Matched
}

// recordAgent is the agent: of the member record a change writes, or "" when
// the file does not parse as a member record (the loader skips such a file).
func recordAgent(ch *object.Change) (string, error) {
	f, err := ch.To.Tree.TreeEntryFile(&ch.To.TreeEntry)
	if err != nil {
		return "", err
	}
	content, err := f.Contents()
	if err != nil {
		return "", err
	}
	ff, err := fact.ParseFact(ch.To.Name, content)
	if err != nil {
		return "", nil
	}
	m, err := fact.ParseMember(ff.Body)
	if err != nil {
		return "", nil
	}
	return m.Agent, nil
}
