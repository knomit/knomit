package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// Audit finding rules (knomit verify audit).
const (
	AuditUnsigned    = "unsigned"     // unsigned or bad signature (an unsigned merge failing M3 included)
	AuditNeverMember = "never-member" // the signing key was never on any member record
	AuditOtherAgent  = "other-agent"  // the key belonged to a different agent than the author claims
	AuditRevoked     = "revoked"      // the key was on a record while that record's state was revoked
)

// AuditInput names the branch to audit and the fleet to judge it against.
type AuditInput struct {
	KBDir     string
	Branch    string // the revision whose whole history is audited
	FleetDir  string
	FleetRev  string // "" = "HEAD"
	FleetRoot string // "" = "kb"
	Key       string // optional: list every commit this key signed (full fingerprint, or a prefix)
}

// AuditReport is the audit's answer.
type AuditReport struct {
	Commits  int       `json:"commits"`
	Findings []Refusal `json:"findings"`
	// KeySigned lists the commits signed by Input.Key, when one was given.
	KeySigned []string `json:"key_signed,omitempty"`
}

// ExitCode: 0 clean, 1 findings. An error from Audit is "could not run" (2).
func (r AuditReport) ExitCode() int {
	if len(r.Findings) > 0 {
		return 1
	}
	return 0
}

// keyVersion is one (agent, state) a key had in one version of the fleet.
type keyVersion struct {
	agent, state string
}

// Audit re-checks a whole branch on demand (the forensic tool: "look at all
// commits signed by that agent from the day we detect the breach backwards").
// Unlike the acceptance gate it resolves each signer against EVERY VERSION of
// the member records in the fleet's history, so a departed agent, or an old
// key since rotated away, is attributed to its agent instead of flagged. It
// never moves a ref and is never in a write path.
func Audit(in AuditInput) (AuditReport, error) {
	fleet, err := openGitDir(in.FleetDir)
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: fleet: %w", err)
	}
	rev := in.FleetRev
	if rev == "" {
		rev = "HEAD"
	}
	root := in.FleetRoot
	if root == "" {
		root = "kb"
	}
	tip, err := resolveCommit(fleet, rev)
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: fleet: %w", err)
	}
	if raw, err := treeOntology(tip); err != nil || raw == nil {
		return AuditReport{}, fmt.Errorf("audit: %w", ErrNotFleet)
	} else if ont, err := fact.ParseOntology(raw); err != nil || !fact.IsFleetOntology(ont) {
		return AuditReport{}, fmt.Errorf("audit: %w", ErrNotFleet)
	}
	// Every version of every record: the fleet's own history is the history
	// of each agent (keys and states), per the fact-history ruling.
	versions := map[string][]keyVersion{} // wire key -> versions
	seenBlob := map[string]bool{}
	err = walkHistory(fleet.Storer, tip.Hash, nil, func(c *object.Commit) {
		ms, lerr := loadMembers(in.FleetDir, c, root)
		if lerr != nil {
			return
		}
		for _, m := range ms {
			if seenBlob[m.Blob] {
				continue
			}
			seenBlob[m.Blob] = true
			k := string(m.Key.Marshal())
			versions[k] = append(versions[k], keyVersion{agent: strings.ToLower(m.Agent), state: m.State})
		}
	})
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: fleet history: %w", err)
	}

	kb, err := openGitDir(in.KBDir)
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: %w", err)
	}
	head, err := resolveCommit(kb, in.Branch)
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: %w", err)
	}
	out := AuditReport{Findings: []Refusal{}}
	want := strings.ToLower(strings.TrimSpace(in.Key))
	err = walkHistory(kb.Storer, head.Hash, map[plumbing.Hash]bool{}, func(c *object.Commit) {
		out.Commits++
		h := c.Hash.String()
		s, serr := verifyCommitSignature(c)
		if serr != nil {
			if (errors.Is(serr, ErrUnsigned) || errors.Is(serr, ErrNotSSHSIG)) && c.NumParents() >= 2 {
				if v, err := checkM3(c); err == nil && v.OK {
					return
				}
			}
			out.Findings = append(out.Findings, Refusal{Commit: h, Rule: AuditUnsigned, Reason: serr.Error()})
			return
		}
		if want != "" && strings.HasPrefix(s.Fingerprint, want) {
			out.KeySigned = append(out.KeySigned, h)
		}
		vs := versions[string(s.Key.Marshal())]
		if len(vs) == 0 {
			out.Findings = append(out.Findings, Refusal{Commit: h, Rule: AuditNeverMember, SignerFP: s.Fingerprint,
				Reason: "the signing key was never on a member record"})
			return
		}
		id := agentIDOfAuthor(c.Author.Email)
		mine, revoked := false, false
		for _, v := range vs {
			if v.agent == id {
				mine = true
				revoked = revoked || v.state == fact.MemberRevoked
			}
		}
		switch {
		case !mine:
			out.Findings = append(out.Findings, Refusal{Commit: h, Rule: AuditOtherAgent, SignerFP: s.Fingerprint,
				Reason: fmt.Sprintf("the key belonged to %q, the author claims %q", vs[0].agent, id)})
		case revoked:
			out.Findings = append(out.Findings, Refusal{Commit: h, Rule: AuditRevoked, SignerFP: s.Fingerprint,
				Reason: fmt.Sprintf("agent %q was revoked while holding this key", id)})
		}
	})
	if err != nil {
		return AuditReport{}, fmt.Errorf("audit: %w", err)
	}
	return out, nil
}
