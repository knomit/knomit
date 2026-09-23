// Package auth defines WHO a request comes from (Principal) and WHAT that
// principal may do (Permission). It has no store dependency and no HTTP
// dependency: internal/web and internal/mcp translate their transports
// into a Principal at the edge, and everything downstream reads only this.
//
// F19 revision 2: three ways in (certificate, token, socket), one principal,
// one permission check. Phase 1 wires the socket and the anonymous
// loopback fallback; certificates and tokens arrive in later phases and
// produce the same type.
package auth

import (
	"context"
	"fmt"
	"strings"
)

type Kind string

const (
	KindInstance  Kind = "instance"
	KindBridge    Kind = "bridge"
	KindHost      Kind = "host"
	KindOperator  Kind = "operator"
	KindAnonymous Kind = "anonymous"
)

type Via string

const (
	ViaCert   Via = "cert"
	ViaToken  Via = "token"
	ViaSocket Via = "socket"
	// ViaPipe is a Windows named pipe: the same shape of credential as
	// ViaSocket -- the OS names the caller, nothing is stored or presented --
	// but a DIFFERENT mechanism, and the grants key carries the difference so
	// a row or a log line can tell a SID read off a pipe from a uid read off
	// a socket. internal/auth.LocalVia picks the one this platform uses.
	ViaPipe Via = "pipe"
	ViaNone Via = "none"
)

// Principal is a verified caller. ID is the stable identifier for the Kind:
// a key fingerprint for an instance, "uid:<n>" or "sid:<SID>" for a local
// peer (auth.LocalPrincipal and Peer.Principal are the only two spellings), a token
// subject for a host. Via records which mechanism vouched for it, so a log
// line or a grants row can tell a kernel-verified uid from a bearer token.
type Principal struct {
	Kind Kind
	ID   string
	Via  Via
}

// String is the grants-table key and the client_sessions.principal value.
// Format: <kind>:<id>@<via>; anonymous has no id and renders "anonymous@none".
func (p Principal) String() string {
	if p.ID == "" {
		return string(p.Kind) + "@" + string(p.Via)
	}
	return string(p.Kind) + ":" + p.ID + "@" + string(p.Via)
}

func (p Principal) IsZero() bool { return p.Kind == "" && p.ID == "" && p.Via == "" }

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal the edge attached, or false when none
// was attached — an in-process caller or a test. Callers that need a
// decision treat false as the zero principal, which Allowed denies.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// ParsePrincipal is the inverse of String: "<kind>:<id>@<via>", or
// "<kind>@<via>" for a principal with no id (anonymous). Kind and Via must be
// ones this package defines, so a typo in `knomit grants add` is an error
// rather than a grants row that names nobody. The id may itself contain ':'
// ("uid:501"): kind ends at the FIRST ':' and via starts after the LAST '@'.
func ParsePrincipal(s string) (Principal, error) {
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return Principal{}, fmt.Errorf("principal %q: want <kind>:<id>@<via>", s)
	}
	head, via := s[:at], Via(s[at+1:])
	kind, id, _ := strings.Cut(head, ":")
	p := Principal{Kind: Kind(kind), ID: id, Via: via}
	switch p.Kind {
	case KindInstance, KindBridge, KindHost, KindOperator, KindAnonymous:
	default:
		return Principal{}, fmt.Errorf("principal %q: unknown kind %q", s, kind)
	}
	// ViaPipe belongs here as much as ViaSocket: on Windows the local
	// principal is ALWAYS bridge:sid:<SID>, so omitting it makes every
	// `knomit grants` command on that platform reject the only local
	// principal there is (knomit#245).
	switch p.Via {
	case ViaCert, ViaToken, ViaSocket, ViaPipe, ViaNone:
	default:
		return Principal{}, fmt.Errorf("principal %q: unknown via %q", s, via)
	}
	if p.ID == "" && p.Kind != KindAnonymous {
		return Principal{}, fmt.Errorf("principal %q: %s needs an id", s, kind)
	}
	// One spelling per principal: "anonymous:@none" parses to the same value
	// as "anonymous@none", and a grants row must not exist under both.
	if p.String() != s {
		return Principal{}, fmt.Errorf("principal %q is not in canonical form %q", s, p.String())
	}
	return p, nil
}
