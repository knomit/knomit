package fact

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Member states. A record is never deleted: leaving or revoking is a change
// of state, and the earlier states stay readable as the fact's history.
const (
	MemberActive  = "active"
	MemberLeft    = "left"
	MemberRevoked = "revoked"
)

// MembersTopic is the fleet ontology's topic for member records. A record
// lives at <root>/members/<agent-id>/<uuid>.md.
const MembersTopic = "members"

// ErrMalformedMember is returned by ParseMember for a record the acceptance
// gate must skip (and log): a missing field, an unknown state, or a key that
// does not parse.
var ErrMalformedMember = errors.New("malformed fleet member record")

// Member is one agent's CURRENT record in a fleet repository. The agent id is
// OPAQUE: nothing parses a fingerprint or a host out of it. Which key the
// agent signs with is Key, compared as the full key, never as a prefix.
type Member struct {
	Agent  string        // the id in the agent's commit author address
	State  string        // active | left | revoked
	Key    ssh.PublicKey // the current signing key
	Host   string        // informational
	Branch string        // informational: the agent branch the instance registered
	Notes  string        // free text after the fields
}

// memberFields are the `field: value` lines a member record's body starts
// with, in the order RenderMember writes them.
var memberFields = []string{"agent", "state", "key", "host", "branch"}

// ParseMember reads a member record's body: one `field: value` line each for
// agent, state and key (required) and host and branch (optional), then free
// text. Unknown fields are ignored so a newer writer's record still parses.
func ParseMember(body string) (Member, error) {
	var m Member
	var keyLine string
	var notes []string
	fieldsDone := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !fieldsDone {
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			k, v, ok := strings.Cut(t, ":")
			if ok && !strings.ContainsAny(k, " \t") {
				switch strings.ToLower(k) {
				case "agent":
					m.Agent = strings.TrimSpace(v)
				case "state":
					m.State = strings.TrimSpace(v)
				case "key":
					keyLine = strings.TrimSpace(v)
				case "host":
					m.Host = strings.TrimSpace(v)
				case "branch":
					m.Branch = strings.TrimSpace(v)
				}
				continue
			}
			fieldsDone = true
		}
		notes = append(notes, line)
	}
	m.Notes = strings.TrimSpace(strings.Join(notes, "\n"))
	if m.Agent == "" {
		return Member{}, fmt.Errorf("%w: no agent", ErrMalformedMember)
	}
	switch m.State {
	case MemberActive, MemberLeft, MemberRevoked:
	case "":
		return Member{}, fmt.Errorf("%w: no state", ErrMalformedMember)
	default:
		return Member{}, fmt.Errorf("%w: unknown state %q", ErrMalformedMember, m.State)
	}
	if keyLine == "" {
		return Member{}, fmt.Errorf("%w: no key", ErrMalformedMember)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyLine))
	if err != nil {
		return Member{}, fmt.Errorf("%w: key: %v", ErrMalformedMember, err)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return Member{}, fmt.Errorf("%w: key type %s, want ssh-ed25519", ErrMalformedMember, pub.Type())
	}
	m.Key = pub
	return m, nil
}

// RenderMember writes m as a member record body, the inverse of ParseMember.
func RenderMember(m Member) string {
	var b strings.Builder
	vals := map[string]string{
		"agent":  m.Agent,
		"state":  m.State,
		"host":   m.Host,
		"branch": m.Branch,
	}
	if m.Key != nil {
		vals["key"] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(m.Key)))
	}
	for _, f := range memberFields {
		if v := vals[f]; v != "" {
			fmt.Fprintf(&b, "%s: %s\n", f, v)
		}
	}
	if m.Notes != "" {
		b.WriteString("\n" + m.Notes + "\n")
	}
	return b.String()
}

// SameKey reports whether a and b are the same public key (the full wire
// encoding; never a fingerprint prefix).
func SameKey(a, b ssh.PublicKey) bool {
	return a != nil && b != nil && bytes.Equal(a.Marshal(), b.Marshal())
}
