package app

import (
	"context"
	"testing"

	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
	"knomit/test/testenv"
)

func seedRowCount(t *testing.T, a *App) int {
	t.Helper()
	var n int
	if err := a.manager.ControlDB().QueryRow(`SELECT COUNT(*) FROM auth_role_permissions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The role seed is written by Go at every boot (control.db up-migrations may
// only CREATE), and the second boot of the same home must find nothing to do:
// the bindings version stays where the first boot left it.
func TestBoot_SeedsRolesOnce(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Home = t.TempDir()

	want := 0
	for _, perms := range authz.Seed {
		want += len(perms)
	}

	a, err := New(ctx, cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if a.authz == nil {
		t.Fatal("boot built no authz materializer")
	}
	if got := seedRowCount(t, a); got != want {
		t.Fatalf("auth_role_permissions has %d rows after boot, want the %d Seed rows", got, want)
	}
	v1, err := authz.NewStore(a.manager.ControlDB()).Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == 0 {
		t.Fatal("the first boot seeded nothing (version 0)")
	}
	// Dark: the owner's policy is total, and nothing else reads it yet.
	owner, err := authz.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := a.authz.For(ctx, owner, authz.ViaSocket); err != nil || !p.Owner {
		t.Fatalf("owner policy = %+v, %v", p, err)
	}
	a.Close()

	a2, err := New(ctx, cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	defer a2.Close()
	if got := seedRowCount(t, a2); got != want {
		t.Fatalf("after reboot: %d rows, want %d", got, want)
	}
	v2, err := authz.NewStore(a2.manager.ControlDB()).Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Fatalf("the second boot moved the bindings version %d -> %d", v1, v2)
	}
}
