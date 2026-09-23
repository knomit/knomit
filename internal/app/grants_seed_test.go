package app

import (
	"context"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// ownLocalPrincipal is who the boot seeds a grant for. It asks
// auth.LocalPrincipal rather than rebuilding the string, because rebuilding
// it is exactly the bug: a test that spelled the principal itself would keep
// passing while the server and the middleware drifted apart (knomit#245).
func ownLocalPrincipal(t *testing.T) auth.Principal {
	t.Helper()
	p, err := auth.LocalPrincipal()
	if err != nil {
		t.Fatalf("LocalPrincipal: %v", err)
	}
	if p.ID == "" {
		t.Fatal("LocalPrincipal produced an empty ID; a grant seeded for it would name nobody")
	}
	return p
}

// The OS user running the server is its operator, so the socket path has to
// work with zero configuration — and a permission the operator later REVOKED
// must not come back at the next restart.
func TestBoot_GrantsOwnUIDLoopbackDefaultOnce(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Home = t.TempDir()

	a, err := New(ctx, cfg, Options{APIOnly: true})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}

	me := ownLocalPrincipal(t)
	set, err := a.server.Grants.For(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has(auth.Write) || !set.Has(auth.Read) {
		t.Fatalf("own uid must hold loopback_default after boot: %v", set)
	}

	sq, ok := a.server.Grants.(*auth.SQLGrants)
	if !ok {
		t.Fatalf("expected *auth.SQLGrants, got %T", a.server.Grants)
	}
	if err := sq.Revoke(ctx, me, auth.Write); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Same home, second New(): the revocation must survive it.
	a2, err := New(ctx, cfg, Options{APIOnly: true})
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	defer a2.Close()

	set, err = a2.server.Grants.For(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	if set.Has(auth.Write) {
		t.Fatal("boot must not re-grant a permission the operator revoked")
	}
	if !set.Has(auth.Read) {
		t.Fatalf("revoking write must not disturb the other seeded permissions: %v", set)
	}
}
