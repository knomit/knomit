// Package authz is knomit's authorization model (unified authn/z design,
// .claude/plans/2026-11-09-unified-auth-design.md): every caller is a
// Principal, a principal's instance-wide Roles materialize a Policy (a set of
// Permissions), and every gate asks Policy.Has.
//
// It is built DARK (plan phase P1): it lives beside the old auth.Grants model
// and no gate consults it yet. The flip (P4) mounts it and deletes the old
// model.
package authz

import (
	"fmt"
	"slices"
)

// Permission is one capability a gate can ask for. The set is a closed code
// registry: a seed row or a binding naming anything else is refused by
// ParsePermission and ignored by the materializer.
type Permission string

const (
	KBRead       Permission = "kb:read"       // query, explain, changes, repos, bind; REST GETs; git fetch
	KBWrite      Permission = "kb:write"      // learn, update, retract, review, hypothesize, experiments; REST fact writes
	ChatUse      Permission = "chat:use"      // the /chats routes (§15.3); every tool call inside is checked on its own
	RepoPush     Permission = "repo:push"     // git push; held by the node role only (§16)
	RepoManage   Permission = "repo:manage"   // repo create/rename/archive/restore; lens CRUD
	RepoOrigin   Permission = "repo:origin"   // origin/upstream/credential; probes; origin sessions; pushed-branch merge
	OpsRun       Permission = "ops:run"       // index rebuilds, jobs, reload; viewing sessions and logs (§15.3)
	SecretManage Permission = "secret:manage" // the secrets store, and linking a stored secret anywhere
	AuthManage   Permission = "auth:manage"   // role bindings, approvals, sessions, tokens
	FleetManage  Permission = "fleet:manage"  // fleet create/enroll/join/leave/revoke/decommission/root/host/follow

	// Public is the route registry's pseudo-permission: the route needs no
	// authentication at all (discovery, login pages, static UI shell,
	// version). No role holds it and ParsePermission refuses it, so it can
	// never be granted; Policy.Has answers true for it for every caller,
	// anonymous and zero policies included.
	Public Permission = "public"
)

// all is the closed set, in its fixed order. Public is not in it.
var all = []Permission{
	KBRead, KBWrite, ChatUse, RepoPush, RepoManage,
	RepoOrigin, OpsRun, SecretManage, AuthManage, FleetManage,
}

// All returns the ten permissions in their fixed order (a fresh copy).
func All() []Permission { return slices.Clone(all) }

// ParsePermission accepts exactly one of the ten spellings. "public" is
// refused: it is a route declaration, never something a role holds.
func ParsePermission(s string) (Permission, error) {
	p := Permission(s)
	if slices.Contains(all, p) {
		return p, nil
	}
	return "", fmt.Errorf("authz: unknown permission %q", s)
}
