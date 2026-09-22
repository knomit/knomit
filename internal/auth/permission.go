package auth

import (
	"context"
	"fmt"
)

type Permission string

const (
	Read      Permission = "read"
	Write     Permission = "write"
	PushOwn   Permission = "push:own"
	MergeMain Permission = "merge:main"
	Operator  Permission = "operator"
	Admin     Permission = "admin"
)

var all = map[Permission]struct{}{
	Read: {}, Write: {}, PushOwn: {}, MergeMain: {}, Operator: {}, Admin: {},
}

type Set map[Permission]struct{}

func (s Set) Has(p Permission) bool { _, ok := s[p]; return ok }

// ParseSet turns config strings into a Set. An unknown name is an ERROR,
// not a no-op: a typo in [auth].loopback_default must fail startup rather
// than silently grant less (or, on a future rename, more) than written.
func ParseSet(names []string) (Set, error) {
	out := Set{}
	for _, n := range names {
		p := Permission(n)
		if _, ok := all[p]; !ok {
			return nil, fmt.Errorf("unknown permission %q", n)
		}
		out[p] = struct{}{}
	}
	return out, nil
}

// Grants answers what a principal holds. Implementations: StaticGrants
// (config/tests) and SQLGrants (control.db). A later phase merges the
// master-signed fleet policy here as well.
type Grants interface {
	For(ctx context.Context, p Principal) (Set, error)
}

// StaticGrants is keyed by Principal.String().
type StaticGrants map[string]Set

func (g StaticGrants) For(_ context.Context, p Principal) (Set, error) {
	return g[p.String()], nil
}

// Allowed is the ONE permission check. Default deny: a zero principal, an
// unknown principal, or a store error all answer false. Errors are not
// surfaced here because no caller can do anything but refuse; the store
// logs its own failures.
func Allowed(ctx context.Context, g Grants, p Principal, perm Permission) bool {
	if p.IsZero() || g == nil {
		return false
	}
	set, err := g.For(ctx, p)
	if err != nil {
		return false
	}
	return set.Has(perm)
}
