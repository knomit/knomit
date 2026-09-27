package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
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
//  2. The fleet's member records are loaded (malformed ones skipped and
//     logged). A missing or unreadable fleet, or one whose ontology is not the
//     fleet preset, is an error: the gate could not run.
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
	members, err := LoadFleet(in.FleetDir, rev, in.FleetRoot)
	if err != nil {
		return RangeVerdict{}, fmt.Errorf("verify: %w", err)
	}
	idx := indexMembers(members)

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
		if r, ok := idx.judgeCommit(c); !ok {
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

// judgeCommit applies the gate's rules to one commit:
//   - an unsigned merge passes only if it adds nothing beyond its parents (M3);
//   - otherwise the SSHSIG must verify;
//   - the author address's agent id must have exactly ONE member record;
//   - the signing key (the full key) must be that record's CURRENT key, and no
//     other record's;
//   - the record's state must be active.
func (idx memberIndex) judgeCommit(c *object.Commit) (Refusal, bool) {
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
	recs := idx.byAgent[id]
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
	if n := len(idx.byKey[string(s.Key.Marshal())]); n > 1 {
		return Refusal{Commit: h, Rule: RuleDuplicateKey, SignerFP: fp, Reason: fmt.Sprintf("the signing key is the current key of %d records", n)}, false
	}
	if rec.State != fact.MemberActive {
		return Refusal{Commit: h, Rule: RuleInactive, SignerFP: fp, Reason: fmt.Sprintf("agent %q is %s", id, rec.State)}, false
	}
	return Refusal{}, true
}
