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

// WithRepoReach records which repos the caller on this request can reach by
// their own routes; reach answers for a repo NAME. EVERY edge that mints a
// principal records one:
//   - the OAuth listener (BearerMiddleware): the verified token's audience. A
//     token is issued for ONE resource (RFC 8707) and is refused anywhere
//     outside it, so a token for one repo's or one lens's endpoint cannot
//     read another repo's routes;
//   - the local edges (AuthMiddleware: socket or named pipe, TLS
//     certificate, loopback, and auth disabled): AllRepos, because grants
//     carry no repo dimension and such a caller can already call every
//     repo's routes.
//
// It exists for reads that leave the request's own repo on the server side
// (materialize's cross-KB refs): such a read must not take a confined token
// further than the token could go itself.
func WithRepoReach(ctx context.Context, reach func(repo string) bool) context.Context {
	return context.WithValue(ctx, repoReachKey{}, reach)
}

// ReachesRepo reports whether the caller's credential reaches the named
// repo's routes. DEFAULT DENY, like Allowed: a context with no recorded reach
// reaches nothing, so a new edge that forgets WithRepoReach (another listener,
// a context detached in a goroutine) fails closed instead of widening to every
// repo. It does NOT replace Allowed: a caller that reaches a repo still needs
// the permission for what it does there.
func ReachesRepo(ctx context.Context, repo string) bool {
	reach, ok := RepoReachFromContext(ctx)
	return ok && reach(repo)
}

// RepoReachFromContext returns the reach WithRepoReach recorded, or false.
func RepoReachFromContext(ctx context.Context) (func(repo string) bool, bool) {
	reach, ok := ctx.Value(repoReachKey{}).(func(string) bool)
	return reach, ok && reach != nil
}

// AllRepos is the reach of a caller no credential confines: every repo.
func AllRepos(string) bool { return true }

type lensReachKey struct{}

// WithLensReach records which lenses the caller on this request can reach by
// their own routes; reach answers for a lens NAME. It is the repo reach's
// sibling, recorded at the same edges:
//   - the OAuth listener (BearerMiddleware): a token for one lens's endpoint
//     reaches that lens, a token for the unscoped MCP endpoint or for the
//     whole API reaches every lens, a token for one repo reaches none;
//   - the local edges (AuthMiddleware): AllLenses.
//
// A lens reach does NOT widen the repo reach: a token confined to lens L
// still reaches no repo by name (ReachesRepo), and L's members are readable
// through L's binding, not through this. What reaching L buys is seeing L —
// and L's own members — where the server lists what a caller may address
// (knomit_repos).
func WithLensReach(ctx context.Context, reach func(lens string) bool) context.Context {
	return context.WithValue(ctx, lensReachKey{}, reach)
}

// ReachesLens reports whether the caller's credential reaches the named
// lens's routes. DEFAULT DENY, exactly like ReachesRepo.
func ReachesLens(ctx context.Context, lens string) bool {
	reach, ok := LensReachFromContext(ctx)
	return ok && reach(lens)
}

// LensReachFromContext returns the reach WithLensReach recorded, or false.
func LensReachFromContext(ctx context.Context) (func(lens string) bool, bool) {
	reach, ok := ctx.Value(lensReachKey{}).(func(string) bool)
	return reach, ok && reach != nil
}

// AllLenses is the lens reach of a caller no credential confines.
func AllLenses(string) bool { return true }

// CeilingFromContext returns the ceiling WithCeiling recorded, or false.
func CeilingFromContext(ctx context.Context) (Set, bool) {
	s, ok := ctx.Value(ceilingKey{}).(Set)
	return s, ok
}
