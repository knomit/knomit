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

import "context"

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
	ViaNone   Via = "none"
)

// Principal is a verified caller. ID is the stable identifier for the Kind:
// a key fingerprint for an instance, "uid:<n>" for a socket peer, a token
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
