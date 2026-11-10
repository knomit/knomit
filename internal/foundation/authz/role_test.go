package authz

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestSeed_MatchesDesignTable pins the role -> permission matrix as a LITERAL:
// design §4, as amended by §15.3 (role guest, permission chat:use) and §16
// (repo:push on node ONLY). Deriving the expectation from Seed would pass
// however wrong Seed was.
func TestSeed_MatchesDesignTable(t *testing.T) {
	want := map[Role][]Permission{
		RoleAdmin:       {KBRead, KBWrite, ChatUse, RepoManage, RepoOrigin, OpsRun, SecretManage, AuthManage, FleetManage},
		RoleOperator:    {KBRead, KBWrite, ChatUse, RepoManage, RepoOrigin, OpsRun},
		RoleContributor: {KBRead, KBWrite, ChatUse},
		RoleReader:      {KBRead, ChatUse},
		RoleGuest:       {KBRead},
		RoleNode:        {KBRead, RepoPush},
	}
	if got := slices.Sorted(maps.Keys(Seed)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
		t.Fatalf("Seed roles = %v, want %v", got, slices.Sorted(maps.Keys(want)))
	}
	for role, perms := range want {
		got := slices.Clone(Seed[role])
		slices.Sort(got)
		w := slices.Clone(perms)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("Seed[%s] = %v, want %v", role, got, w)
		}
	}

	// The rulings spelled out, so a failure names the rule that broke.
	for role, perms := range Seed {
		if slices.Contains(perms, RepoPush) != (role == RoleNode) {
			t.Errorf("§16: repo:push is held by node ONLY; %s has it = %v", role, slices.Contains(perms, RepoPush))
		}
		wantChat := role == RoleAdmin || role == RoleOperator || role == RoleContributor || role == RoleReader
		if slices.Contains(perms, ChatUse) != wantChat {
			t.Errorf("§15.3: chat:use is reader and above, not guest or node; %s has it = %v", role, !wantChat)
		}
		if slices.Contains(perms, Public) {
			t.Errorf("public is a route pseudo-permission; role %s must not hold it", role)
		}
	}
	if !slices.Equal(Seed[RoleGuest], []Permission{KBRead}) {
		t.Errorf("§15.3: guest = kb:read only, got %v", Seed[RoleGuest])
	}
}

func TestParseRole_OwnerIsNotARole(t *testing.T) {
	_, err := ParseRole("owner")
	if err == nil {
		t.Fatal(`ParseRole("owner") accepted: the owner is implicit, never a role`)
	}
	if !strings.Contains(err.Error(), "owner") {
		t.Fatalf("the refusal should say why: %v", err)
	}
	for r := range Seed {
		got, err := ParseRole(string(r))
		if err != nil || got != r {
			t.Fatalf("ParseRole(%q) = %q, %v", r, got, err)
		}
	}
	for _, bad := range []string{"", "Admin", "root", "write", "anonymous"} {
		if _, err := ParseRole(bad); err == nil {
			t.Fatalf("ParseRole(%q) accepted", bad)
		}
	}
}

func TestTokenRoles_NeverAdminNorGuestNorNode(t *testing.T) {
	want := []Role{RoleReader, RoleContributor, RoleOperator}
	if got := TokenRoles(); !slices.Equal(got, want) {
		t.Fatalf("TokenRoles() = %v, want %v", got, want)
	}
}
