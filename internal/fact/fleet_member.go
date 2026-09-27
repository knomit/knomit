package fact

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
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
	// F10 advertised fields. Addresses are the operator's external_addresses
	// verbatim; Git is the path suffix a peer appends to one ("" when the
	// instance does not serve /git); Capabilities are only what the instance
	// detects on its own host (os, arch, version, read_only). Someone else's
	// values are kept as written: nothing here is validated or used yet.
	Addresses    []string
	Git          string
	Capabilities map[string]string
	Notes        string // free text after the fields
}

// memberFields are the `field: value` lines a member record's body starts
// with, in the order RenderMember writes them.
var memberFields = []string{"agent", "state", "key", "host", "branch", "addresses", "git", "capabilities"}

// ParseMember reads a member record's body: one `field: value` line each for
// agent, state and key (required) and host, branch, addresses, git and
// capabilities (optional), then free text. Unknown fields are ignored so a newer writer's record still parses.
func ParseMember(body string) (Member, error) {
	m := Member{Addresses: []string{}}
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
				case "addresses":
					m.Addresses = append([]string{}, strings.Fields(v)...)
				case "git":
					m.Git = strings.TrimSpace(v)
				case "capabilities":
					m.Capabilities = parseCapabilities(v)
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
		"agent":        m.Agent,
		"state":        m.State,
		"host":         m.Host,
		"branch":       m.Branch,
		"addresses":    strings.Join(m.Addresses, " "),
		"git":          m.Git,
		"capabilities": renderCapabilities(m.Capabilities),
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

// parseCapabilities reads `k=v` tokens. A token without `=` (or with an empty
// key) is kept whole as a key with an empty value: displayed as written.
func parseCapabilities(v string) map[string]string {
	out := map[string]string{}
	for _, tok := range strings.Fields(v) {
		k, val, ok := strings.Cut(tok, "=")
		if !ok || k == "" {
			out[tok] = ""
			continue
		}
		out[k] = val
	}
	return out
}

// renderCapabilities writes sorted `k=v` tokens, so an unchanged instance
// renders the same bytes at every boot.
func renderCapabilities(c map[string]string) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	toks := make([]string, 0, len(keys))
	for _, k := range keys {
		if c[k] == "" {
			toks = append(toks, k)
			continue
		}
		toks = append(toks, k+"="+c[k])
	}
	return strings.Join(toks, " ")
}

// AdvertisedDiff names the advertised fields (key, host, branch, addresses,
// git, capabilities) in which m and o differ, in record order; nil when none.
// Agent, state and notes are NOT advertised: the instance never rewrites them
// on its own (state changes only by registering and unregistering).
func (m Member) AdvertisedDiff(o Member) []string {
	var d []string
	if !(m.Key == nil && o.Key == nil) && !SameKey(m.Key, o.Key) {
		d = append(d, "key")
	}
	if m.Host != o.Host {
		d = append(d, "host")
	}
	if m.Branch != o.Branch {
		d = append(d, "branch")
	}
	if strings.Join(m.Addresses, " ") != strings.Join(o.Addresses, " ") {
		d = append(d, "addresses")
	}
	if m.Git != o.Git {
		d = append(d, "git")
	}
	if renderCapabilities(m.Capabilities) != renderCapabilities(o.Capabilities) {
		d = append(d, "capabilities")
	}
	return d
}

// SameAdvertised reports whether m and o agree on every advertised field.
func (m Member) SameAdvertised(o Member) bool { return len(m.AdvertisedDiff(o)) == 0 }
