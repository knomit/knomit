package store

import (
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
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
}

// RangeVerdict is the acceptance gate's answer for one candidate.
type RangeVerdict struct {
	Mode    string    `json:"mode"`    // verify_signatures at Main's tip
	Checked int       `json:"checked"` // commits in the candidate range
	Refused []Refusal `json:"refused"`
}

// ExitCode is the gate's contract for a CI job: 0 mergeable (verification
// off, a clean range, or failures in log mode, which are reported only), 1
// blocked (failures in enforce). "Could not run" (2) is an error from
// CheckRange, never a verdict.
func (v RangeVerdict) ExitCode() int {
	if v.Mode == VerifyEnforce && len(v.Refused) > 0 {
		return 1
	}
	return 0
}

// agentIDOfAuthor returns the agent id in an agent author address,
// <id>[+<op>]@agents.knomit.io, or "" for any other address. The id is OPAQUE:
// it is looked up, never parsed further.
func agentIDOfAuthor(email string) string {
	local, ok := strings.CutSuffix(strings.ToLower(strings.TrimSpace(email)), "@agents.knomit.io")
	if !ok || local == "" {
		return ""
	}
	id, _, _ := strings.Cut(local, "+")
	return id
}

// CheckRange is F09's acceptance gate: it runs ONCE, where a change enters a
// knowledge base's main (the merge job, via `knomit verify ci`), over plain git
// checkouts so it needs no knomit instance.
//
//  1. verify_signatures is read at Main's tip FIRST. Off or absent: the verdict
//     is off and nothing else runs; the fleet is never touched.
//  2. The fleet's member records and certificate artifacts (root.crt, crl.pem,
//     certs/<id>.pem under artifacts/fleet/) are loaded from the ONE fleet
//     revision (malformed records skipped and logged). A missing or unreadable
//     fleet, or one whose ontology is not the fleet preset, is an error: the
//     gate could not run. Missing artifacts are not an error: no PKI.
//  3. Every commit in Candidate ∖ Main is judged by judgeCommit.
func CheckRange(in RangeInput) (RangeVerdict, error) {
	kb, err := openGitDir(in.KBDir)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	mainC, err := resolveCommit(kb, in.Main)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	raw, err := treeOntology(mainC)
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
	if settings.Mode == VerifyOff {
		return out, nil
	}

	if in.FleetDir == "" {
		return RangeVerdict{}, ErrNoFleet
	}
	rev := in.FleetRev
	if rev == "" {
		rev = "HEAD"
	}
	fleet, err := LoadFleetAt(in.FleetDir, rev, in.FleetRoot)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	// Rule A (self-registration) exists only where member records live: the
	// knowledge base being judged must itself be the fleet repository.
	selfRoot := ""
	if ont, perr := fact.ParseOntology(raw); raw != nil && perr == nil && fact.IsFleetOntology(ont) {
		if selfRoot = in.FleetRoot; selfRoot == "" {
			selfRoot = "kb"
		}
	}
	gate := newJudge(fleet, selfRoot)

	candC, err := resolveCommit(kb, in.Candidate)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := walkHistory(kb.Storer, mainC.Hash, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	if onMain[candC.Hash] {
		return out, nil
	}
	err = walkHistory(kb.Storer, candC.Hash, onMain, func(c *object.Commit) {
		out.Checked++
		if r, ok := gate.judgeCommit(c); !ok {
			out.Refused = append(out.Refused, r)
		}
	})
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	return out, nil
}

// memberIndex is the fleet's member records, by agent id and by current key.
type memberIndex struct {
	byAgent map[string][]FleetMember
	byKey   map[string][]FleetMember // wire key -> records whose CURRENT key it is
}

func indexMembers(ms []FleetMember) memberIndex {
	idx := memberIndex{byAgent: map[string][]FleetMember{}, byKey: map[string][]FleetMember{}}
	for _, m := range ms {
		idx.byAgent[strings.ToLower(m.Agent)] = append(idx.byAgent[strings.ToLower(m.Agent)], m)
		k := string(m.Key.Marshal())
		idx.byKey[k] = append(idx.byKey[k], m)
	}
	return idx
}

// judge is everything judgeCommit reads from the fleet at ONE revision: the
// member records, the certificate artifacts, and where the judged
// repository's member records live.
type judge struct {
	memberIndex
	pki *FleetPKI // nil: no artifacts/fleet/root.crt at the fleet revision
	// membersDir is "<root>/members/" of the JUDGED repository when that
	// repository is the fleet repository itself, else "" (rule A off: there
	// is nothing to self-register into).
	membersDir string
}

// newJudge builds the judge for one fleet revision. selfRoot is the judged
// repository's fact root when that repository is the fleet repository, and
// "" when it is not (rule A off).
func newJudge(f Fleet, selfRoot string) judge {
	j := judge{memberIndex: indexMembers(f.Members), pki: f.PKI}
	if selfRoot != "" {
		j.membersDir = strings.ToLower(path.Join(selfRoot, fact.MembersTopic)) + "/"
	}
	return j
}

// judgeCommit applies the gate's rules to one commit:
//   - an unsigned merge passes only if it adds nothing beyond its parents (M3);
//   - otherwise the SSHSIG must verify;
//   - the author address's agent id must have exactly ONE member record;
//   - the signing key (the full key) must be that record's CURRENT key, and no
//     other record's;
//   - the record's state must be active;
//   - rule B: the leaf artifact for the id at the fleet revision must not be a
//     revoked leaf for that key (RuleRevoked);
//   - rule A: a commit refused above for having no record, a record for
//     another key, or an inactive record is ADMITTED when it is a
//     self-registration backed by a valid leaf artifact (selfRegistration).
//
// Nothing is derived from the id's text (D18): it is only the key under which
// a record or a leaf file is looked up.
func (j judge) judgeCommit(c *object.Commit) (Refusal, bool) {
	h := c.Hash.String()
	s, serr := verifyCommitSignature(c)
	if serr != nil {
		if (errors.Is(serr, ErrUnsigned) || errors.Is(serr, ErrNotSSHSIG)) && c.NumParents() >= 2 {
			v, err := checkM3(c)
			if err != nil {
				return Refusal{Commit: h, Rule: RuleMerge, Reason: err.Error()}, false
			}
			if !v.OK {
				return Refusal{Commit: h, Rule: RuleMerge, Reason: v.Reason}, false
			}
			return Refusal{}, true
		}
		return Refusal{Commit: h, Rule: RuleSignature, Reason: serr.Error()}, false
	}
	fp := s.Fingerprint
	id := agentIDOfAuthor(c.Author.Email)
	if id == "" {
		return Refusal{Commit: h, Rule: RuleNoRecord, SignerFP: fp, Reason: fmt.Sprintf("author %q is not an agent address", c.Author.Email)}, false
	}
	r, ok := j.judgeRecord(h, id, s)
	switch {
	case ok:
		return j.revoked(h, id, s)
	case r.Rule == RuleNoRecord || r.Rule == RuleKeyMismatch || r.Rule == RuleInactive:
		return j.selfRegistration(c, id, s, r)
	}
	return r, false
}

// judgeRecord is the record rules: exactly one record at id, whose current key
// is the signing key and no other record's, in state active.
func (j judge) judgeRecord(h, id string, s CommitSigner) (Refusal, bool) {
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

// revoked is rule B, for a commit the record rules accepted (so the record's
// key IS the signing key): it is refused when the leaf artifact for id at the
// fleet revision names that key, chains to the root, and its serial is on a
// CRL the root signed. A leaf for another key, a leaf the root did not sign,
// or a CRL that cannot be read revokes nothing: a revocation is believed only
// when the root says it.
func (j judge) revoked(h, id string, s CommitSigner) (Refusal, bool) {
	leaf, err := j.leafFor(id, s)
	if leaf == nil || !errors.Is(err, pki.ErrRevoked) {
		return Refusal{}, true
	}
	return Refusal{Commit: h, Rule: RuleRevoked, SignerFP: s.Fingerprint,
		Reason: fmt.Sprintf("the certificate for agent %q (serial %s) is on the fleet CRL", id, leaf.SerialNumber.Text(16))}, false
}

// selfRegistration is rule A. The commit was refused by the record rules
// with refusal; it is ADMITTED instead when:
//   - every path it changes, against EVERY parent, is under the judged fleet
//     repository's members/<id>/ (the author writing its own record and
//     nothing else), and
//   - the leaf artifact certs/<id>.pem at the fleet revision exists, its key
//     equals the SIGNING key (full key), it chains to root.crt and its serial
//     is not on crl.pem.
//
// A commit of any other shape keeps refusal unchanged. A self-registration
// with no leaf, or one that does not chain, keeps refusal's rule with the
// reason; a leaf for another key is RuleKeyMismatch; a revoked leaf is
// RuleRevoked.
func (j judge) selfRegistration(c *object.Commit, id string, s CommitSigner, refusal Refusal) (Refusal, bool) {
	if j.pki == nil || j.membersDir == "" || !onlyUnder(c, j.membersDir+id+"/") {
		return refusal, false
	}
	leaf, err := j.leafFor(id, s)
	switch {
	case leaf == nil && err == nil:
		refusal.Reason += fmt.Sprintf("; self-registration not admitted: no %s/%s.pem at the fleet revision", fleetCertsDir, id)
	case errors.Is(err, errLeafOtherKey):
		refusal.Rule = RuleKeyMismatch
		refusal.Reason = fmt.Sprintf("self-registration not admitted: %s/%s.pem is for another key than the signing key", fleetCertsDir, id)
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
func (j judge) leafFor(id string, s CommitSigner) (*x509.Certificate, error) {
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
	if j.pki.Root == nil {
		return leaf, j.pki.RootErr
	}
	if j.pki.CRL == nil {
		return leaf, j.pki.CRLErr
	}
	at := leaf.NotBefore
	if j.pki.Root.NotBefore.After(at) {
		at = j.pki.Root.NotBefore
	}
	_, err = pki.VerifyInstanceChain(leaf, nil, j.pki.Root, j.pki.CRL, at, pki.UsageClient)
	return leaf, err
}

// onlyUnder reports whether c changes at least one path and every path it
// changes against EVERY parent (the empty tree for a root commit) is under
// dir (lowercased, ending in "/"). A merge that brings in anything else from
// one side is not under dir against the other side. An unreadable tree is
// "no": the commit falls back to the record rules.
func onlyUnder(c *object.Commit, dir string) bool {
	tree, err := c.Tree()
	if err != nil {
		return false
	}
	parents := []*object.Tree{nil}
	if c.NumParents() > 0 {
		parents = parents[:0]
		for i := range c.NumParents() {
			p, err := c.Parent(i)
			if err != nil {
				return false
			}
			pt, err := p.Tree()
			if err != nil {
				return false
			}
			parents = append(parents, pt)
		}
	}
	changed := false
	for _, pt := range parents {
		changes, err := object.DiffTree(pt, tree)
		if err != nil {
			return false
		}
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if name == "" {
					continue
				}
				if !strings.HasPrefix(strings.ToLower(name), dir) {
					return false
				}
				changed = true
			}
		}
	}
	return changed
}
