package auth

import "context"

// TokenGrants narrows a bearer-token principal to what its token was
// approved for: grants(principal) ∩ ceiling. The rows say what the SUBJECT
// may do on this instance; the ceiling, carried on the request context by
// the bearer middleware that verified the token, says what THIS TOKEN may
// do. A token principal with no ceiling on the context holds nothing — it
// was not verified on this request. Every other principal passes through.
//
// It mirrors CertGrants: one Grants wrapper per credential kind, composed in
// web.Server.grants(), so auth.Allowed stays the only check.
type TokenGrants struct{ Inner Grants }

func (g TokenGrants) For(ctx context.Context, p Principal) (Set, error) {
	var rows Set
	if g.Inner != nil {
		var err error
		if rows, err = g.Inner.For(ctx, p); err != nil {
			return nil, err // Allowed denies on a store error; so must we
		}
	}
	if p.Via != ViaToken {
		return rows, nil
	}
	ceiling, _ := CeilingFromContext(ctx)
	out := Set{}
	for perm := range rows {
		if ceiling.Has(perm) {
			out[perm] = struct{}{}
		}
	}
	return out, nil
}

type ceilingKey struct{}

// WithCeiling records the permissions the verified bearer token on this
// request may exercise.
func WithCeiling(ctx context.Context, s Set) context.Context {
	return context.WithValue(ctx, ceilingKey{}, s)
}

// CeilingFromContext returns the ceiling WithCeiling recorded, or false.
func CeilingFromContext(ctx context.Context) (Set, bool) {
	s, ok := ctx.Value(ceilingKey{}).(Set)
	return s, ok
}
