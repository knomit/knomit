package authz

import (
	"fmt"
	"strings"
	"unicode"

	"knomit/internal/foundation/auth"
)

// Kind is what sort of caller a Principal is.
type Kind string

const (
	KindOS        Kind = "os"        // a local OS account, read off the socket or pipe by the kernel
	KindToken     Kind = "token"     // one credential: a personal access token or an OAuth family
	KindNode      Kind = "node"      // a fleet node, by its instance certificate's fingerprint
	KindAnonymous Kind = "anonymous" // nothing was presented
)

// Principal is WHO is calling, derived by the server from the channel and
// never claimed by the client.
//
// Identity is (Kind, ID) and nothing else: String() omits Label, and
// ParsePrincipal never fills it. Label is a human name for display ("kb on
// laptop"). Unlike the old auth.Principal there is no transport in the
// identity: how a caller connected is Policy.Via, for audit only.
type Principal struct {
	Kind  Kind
	ID    string
	Label string
}

// String is the one persisted spelling: "os:uid:1000", "os:sid:S-1-5-21-…",
// "token:<id>", "node:<fingerprint>", or "anonymous".
func (p Principal) String() string {
	if p.Kind == KindAnonymous {
		return string(KindAnonymous)
	}
	return string(p.Kind) + ":" + p.ID
}

// identity is the comparable identity of p, Label dropped.
func (p Principal) identity() Principal { return Principal{Kind: p.Kind, ID: p.ID} }

// Anonymous is the principal of a request that presented no credential.
func Anonymous() Principal { return Principal{Kind: KindAnonymous} }

// ParsePrincipal is String's inverse. It is strict: a spelling it accepts is
// one String produces, so a persisted principal round-trips exactly.
func ParsePrincipal(s string) (Principal, error) {
	if s == string(KindAnonymous) {
		return Anonymous(), nil
	}
	kind, id, ok := strings.Cut(s, ":")
	if !ok {
		return Principal{}, fmt.Errorf("authz: malformed principal %q", s)
	}
	p := Principal{Kind: Kind(kind), ID: id}
	switch p.Kind {
	case KindOS:
		if !validOSID(id) {
			return Principal{}, fmt.Errorf("authz: malformed os principal %q (want os:uid:<n> or os:sid:<SID>)", s)
		}
	case KindToken, KindNode:
		if !validOpaqueID(id) {
			return Principal{}, fmt.Errorf("authz: malformed %s principal %q", kind, s)
		}
	default:
		return Principal{}, fmt.Errorf("authz: unknown principal kind in %q", s)
	}
	return p, nil
}

// validOSID accepts exactly what auth's per-platform localID formats, in its
// ONE spelling: "uid:<decimal>" on unix (strconv.Itoa: no sign, no leading
// zero) and "sid:S-1-<authority>-<subauthority>…" on Windows
// (ConvertSidToStringSid: revision 1, decimal components, none empty or
// zero-padded). A second spelling of one account would be a second
// principal: a binding made for one would not match a request naming the
// other.
func validOSID(id string) bool {
	if n, ok := strings.CutPrefix(id, "uid:"); ok {
		return canonicalDecimal(n)
	}
	if sid, ok := strings.CutPrefix(id, "sid:"); ok {
		rest, ok := strings.CutPrefix(sid, "S-1-")
		if !ok {
			return false
		}
		for _, part := range strings.Split(rest, "-") {
			if !canonicalDecimal(part) {
				return false
			}
		}
		return true
	}
	return false
}

// canonicalDecimal: a non-empty run of ASCII digits with no leading zero
// (except "0" itself).
func canonicalDecimal(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// canonical reports whether p is in its one persisted spelling: it parses,
// and parses back to the same identity (an anonymous principal carrying an
// ID, for example, stringifies to "anonymous" but is not the anonymous
// principal).
func canonical(p Principal) bool {
	c, err := ParsePrincipal(p.String())
	return err == nil && c == p.identity()
}

// validOpaqueID accepts a non-empty id of printable, non-space characters
// with no "@" (which the old <kind>:<id>@<via> spelling used, so an old
// string can never parse as a new principal).
func validOpaqueID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r == '@' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// FromPeer names the caller on a local (socket or pipe) connection. The ID is
// the kernel's answer exactly as auth formats it, so it has the one spelling
// Owner also uses (fact invariants/auth/local-principal-has-one-spelling):
// never call os.Getuid here.
func FromPeer(p auth.Peer) Principal {
	return Principal{Kind: KindOS, ID: p.ID}
}

// Owner is the principal of the OS account running this server: the implicit,
// total, unbindable owner. It comes from auth.LocalPrincipal, the same
// per-platform formatter FromPeer's input comes from, so a request over our
// own socket is the owner and nothing else is.
func Owner() (Principal, error) {
	lp, err := auth.LocalPrincipal()
	if err != nil {
		return Principal{}, fmt.Errorf("authz: resolve the owner: %w", err)
	}
	return Principal{Kind: KindOS, ID: lp.ID}, nil
}
