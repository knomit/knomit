package authz

import (
	"context"
	"slices"
)

// Via is how a request reached the server. It is for audit and logs ONLY:
// no gate may consult it (design §2). A gate asks Policy.Has.
type Via string

const (
	ViaSocket  Via = "socket"  // unix domain socket, kernel peer credentials
	ViaPipe    Via = "pipe"    // Windows named pipe, kernel peer credentials
	ViaSession Via = "session" // browser session cookie
	ViaBearer  Via = "bearer"  // Authorization: Bearer
	ViaBasic   Via = "basic"   // Basic with a token as the password (git clients)
	ViaMTLS    Via = "mtls"    // fleet node client certificate
	ViaNone    Via = "none"    // nothing presented
)

// Policy is a principal's materialized permission set. It is a value: a
// caller holds it for one request (or one tool call) and asks Has.
//
// The zero Policy denies everything except Public, so a missing policy fails
// closed.
type Policy struct {
	Principal Principal
	Via       Via // audit only, never an input to Has
	Owner     bool
	Roles     []Role // the roles that contributed, sorted; empty for the owner
	// Version is the bindings version this policy was materialized at. A long
	// session (chat, MCP) compares it with Store.Version to notice a change.
	// 0 for the owner, whose policy never changes.
	Version int64

	perms map[Permission]bool
}

// Has reports whether the policy grants perm. Public is granted to everyone;
// the owner is granted every KNOWN permission (an unknown or empty one is
// denied even to the owner, so a typo in a gate fails closed); otherwise
// perm must be in the set.
func (p Policy) Has(perm Permission) bool {
	if perm == Public {
		return true
	}
	if p.Owner {
		return slices.Contains(all, perm)
	}
	return p.perms[perm]
}

// Permissions lists what the policy grants, in All()'s order. Public is not
// listed.
func (p Policy) Permissions() []Permission {
	out := []Permission{}
	for _, perm := range all {
		if p.Has(perm) {
			out = append(out, perm)
		}
	}
	return out
}

type policyKey struct{}

// WithPolicy attaches a materialized policy to ctx.
func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p)
}

// PolicyFrom returns the policy attached to ctx, or false when there is none.
// A gate that finds none must deny (the zero Policy does).
func PolicyFrom(ctx context.Context) (Policy, bool) {
	p, ok := ctx.Value(policyKey{}).(Policy)
	return p, ok
}
