package auth

import "context"

// CertGrants gives every chained INSTANCE `read` and `push:own` implicitly
// and takes everything else from Store. The chain to the fleet root IS
// admission: an instance the operator enrolled may read, which is what fleet
// membership means (F10's admitted-fingerprint list, replaced by the
// certificate). `push:own` joined `read` by the user's F11 ruling
// (2026-09-28, "1. A"): it lets a peer update ONLY its own agent branch on
// this host, and a per-peer grant was a step the operator would always take.
// `write`, `merge:main`, `operator` and `admin` stay grants rows.
//
// Only KindInstance via a certificate gets the implicit read. An operator
// certificate is a different principal and holds exactly its grants rows.
//
// There is nothing to revoke on this side: a revoked certificate never
// reaches a Grants lookup, because the TLS handshake refused it. To remove an
// instance, revoke its certificate; `knomit grants revoke … read` on an
// instance is refused for that reason.
type CertGrants struct{ Store Grants }

func (g CertGrants) For(ctx context.Context, p Principal) (Set, error) {
	var rows Set
	if g.Store != nil {
		var err error
		if rows, err = g.Store.For(ctx, p); err != nil {
			return nil, err // Allowed denies on a store error; so must we
		}
	}
	if p.Kind != KindInstance || p.Via != ViaCert || p.ID == "" {
		return rows, nil
	}
	out := Set{Read: {}, PushOwn: {}}
	for perm := range rows {
		out[perm] = struct{}{}
	}
	return out, nil
}
