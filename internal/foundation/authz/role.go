package authz

import (
	"fmt"
	"slices"
)

// Role is a named, fixed bundle of permissions. Roles are seed data written
// by Store.SeedRoles at boot; users cannot define roles. The owner is NOT a
// role: it is implicit, total and not bindable (ParseRole refuses "owner").
type Role string

const (
	RoleAdmin       Role = "admin"
	RoleOperator    Role = "operator"
	RoleContributor Role = "contributor"
	RoleReader      Role = "reader"
	RoleGuest       Role = "guest"
	RoleNode        Role = "node"
)

// Seed is the role -> permission table (design §4 as amended by §15.3 and
// §16). It is the single authority: SeedRoles makes control.db match it
// exactly at every boot, adding what is missing and removing what is not
// here, so a release that takes a permission away from a role really takes
// it away.
//
//   - repo:push is held by node ONLY (§16). Admin, operator and contributor
//     do not hold it: git push by tokens is parked for a later project.
//   - chat:use is held by reader and above (admin, operator, contributor,
//     reader), not by guest and not by node (§15.3).
//   - guest = kb:read only; it is the only role anonymous may hold (§15.3).
//   - node = kb:read + repo:push, and is never bound: it comes from the
//     fleet-root-signed instance certificate itself (§13.1).
var Seed = map[Role][]Permission{
	RoleAdmin:       {KBRead, KBWrite, ChatUse, RepoManage, RepoOrigin, OpsRun, SecretManage, AuthManage, FleetManage},
	RoleOperator:    {KBRead, KBWrite, ChatUse, RepoManage, RepoOrigin, OpsRun},
	RoleContributor: {KBRead, KBWrite, ChatUse},
	RoleReader:      {KBRead, ChatUse},
	RoleGuest:       {KBRead},
	RoleNode:        {KBRead, RepoPush},
}

// ParseRole accepts the name of a seeded role. "owner" is refused with its
// own message: the owner is implicit (the OS account running the server),
// never something bound.
func ParseRole(s string) (Role, error) {
	if s == "owner" {
		return "", fmt.Errorf("authz: owner is not a role: the owner is the OS account running the server, implicit and never bound")
	}
	r := Role(s)
	if _, ok := Seed[r]; ok {
		return r, nil
	}
	return "", fmt.Errorf("authz: unknown role %q", s)
}

// tokenRoles are the roles a token principal (a PAT or an OAuth family) may be
// bound to (§13.2): exactly one of these. Never admin (a token is not the
// owner's equal), never guest (anonymous-only), never node (certificate-only).
var tokenRoles = []Role{RoleReader, RoleContributor, RoleOperator}

// TokenRoles returns the roles a token may be bound to, weakest first.
func TokenRoles() []Role { return slices.Clone(tokenRoles) }
