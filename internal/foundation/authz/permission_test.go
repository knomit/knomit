package authz

import (
	"slices"
	"testing"
)

// The ten permissions, in the order the plan fixes (design §3 as amended by
// §15.3: chat:use is a permission of its own). The order is part of the
// contract: Policy.Permissions and the UI list them in it.
func TestPermissions_TenInOrder(t *testing.T) {
	want := []Permission{
		"kb:read", "kb:write", "chat:use", "repo:push", "repo:manage",
		"repo:origin", "ops:run", "secret:manage", "auth:manage", "fleet:manage",
	}
	if got := All(); !slices.Equal(got, want) {
		t.Fatalf("All() = %v\nwant     %v", got, want)
	}
	// All returns a copy: a caller appending to or sorting it must not
	// reorder the registry for everyone else.
	a := All()
	a[0] = "x:y"
	if All()[0] != KBRead {
		t.Fatal("All() exposes its backing array")
	}
}

func TestParsePermission(t *testing.T) {
	for _, p := range All() {
		got, err := ParsePermission(string(p))
		if err != nil || got != p {
			t.Fatalf("ParsePermission(%q) = %q, %v", p, got, err)
		}
	}
	// public is a pseudo-permission of the route registry: no role holds it,
	// so it must never parse as something a seed row or a binding could name.
	for _, bad := range []string{"", "public", "KB:READ", "kb:read ", "read", "write", "merge:main", "push:own", "x:y"} {
		if _, err := ParsePermission(bad); err == nil {
			t.Fatalf("ParsePermission(%q) accepted", bad)
		}
	}
}
