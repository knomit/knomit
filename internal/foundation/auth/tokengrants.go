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

type repoReachKey struct{}

// WithRepoReach records which repos the verified bearer token on this request
// can reach by their own routes: a token is issued for ONE resource (RFC
// 8707) and is refused anywhere outside it, so a token for one repo's or one
// lens's endpoint cannot read another repo's routes. reach answers for a
// repo NAME.
//
// It exists for reads that leave the request's own repo on the server side
// (materialize's cross-KB refs): such a read must not take a confined token
// further than the token could go itself.
func WithRepoReach(ctx context.Context, reach func(repo string) bool) context.Context {
	return context.WithValue(ctx, repoReachKey{}, reach)
}

// ReachesRepo reports whether the caller's credential reaches the named
// repo's routes. With no reach recorded it is true: only the OAuth listener
// confines a credential to a resource, and grants (Allowed) carry no repo
// dimension, so a caller on any other transport can already read every repo
// the server holds. It does NOT replace Allowed: a caller that reaches a repo
// still needs the permission for what it does there.
func ReachesRepo(ctx context.Context, repo string) bool {
	reach, ok := ctx.Value(repoReachKey{}).(func(string) bool)
	return !ok || reach(repo)
}

// CeilingFromContext returns the ceiling WithCeiling recorded, or false.
func CeilingFromContext(ctx context.Context) (Set, bool) {
	s, ok := ctx.Value(ceilingKey{}).(Set)
	return s, ok
}
