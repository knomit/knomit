package authz

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// MaterializerOptions are the in-memory policy inputs that are never stored.
type MaterializerOptions struct {
	// AnonymousRoles are roles anonymous holds in memory, never in the
	// database: read-only (demo) mode passes [guest] (§14.4, §15.3). Any role
	// but guest is ignored, as anonymous may hold guest only.
	AnonymousRoles []Role
}

type cached struct {
	version int64
	roles   []Role
	perms   map[Permission]bool
}

// Materializer turns a principal into its Policy: the owner gets everything
// with no store access; anyone else gets the union of the permissions of
// their stored bindings, the implicit roles the authenticator passes (a fleet
// node's certificate implies RoleNode) and, for anonymous, the in-memory
// AnonymousRoles.
//
// Results are cached per (principal, implicit roles). Each For reads
// Store.Version with ONE SELECT and uses a cached entry only while its version
// equals that, so a Bind, Unbind or reseed takes effect on the very next
// call. A store error denies: the returned Policy grants nothing (but Public)
// and the error is returned, whatever was cached.
type Materializer struct {
	store *Store
	owner Principal
	anon  []Role

	mu           sync.Mutex
	cacheVersion int64
	cache        map[string]cached
}

// NewMaterializer builds a materializer over s. owner is the server's own OS
// principal (Owner()); a zero or non-OS owner matches nobody.
func NewMaterializer(s *Store, owner Principal, o MaterializerOptions) *Materializer {
	var anon []Role
	for _, r := range o.AnonymousRoles {
		if r != RoleGuest {
			log.Warn().Str("role", string(r)).Msg("authz: anonymous may hold guest only; ignoring this in-memory role")
			continue
		}
		if !slices.Contains(anon, r) {
			anon = append(anon, r)
		}
	}
	return &Materializer{store: s, owner: owner.identity(), anon: anon, cache: map[string]cached{}}
}

func (m *Materializer) isOwner(p Principal) bool {
	return m.owner.Kind == KindOS && m.owner.ID != "" && p.identity() == m.owner
}

// admits reports whether p may hold role r at all, however it got it: a
// role outside the seed, a node role on anything but a node, and anything
// but guest on anonymous are dropped (defence in depth: Bind refuses them,
// a hand-edited row or a careless caller must not revive them).
func admits(p Principal, r Role) bool {
	if _, ok := Seed[r]; !ok {
		return false
	}
	if r == RoleNode && p.Kind != KindNode {
		return false
	}
	if p.Kind == KindAnonymous && r != RoleGuest {
		return false
	}
	return true
}

// For materializes p's policy. via is recorded for audit only. implicit are
// roles the authenticator vouches for without a binding (RoleNode for a
// fleet node's certificate).
func (m *Materializer) For(ctx context.Context, p Principal, via Via, implicit ...Role) (Policy, error) {
	pol := Policy{Principal: p, Via: via}
	if m.isOwner(p) {
		pol.Owner = true
		return pol, nil
	}

	extra := slices.Clone(implicit)
	if p.Kind == KindAnonymous {
		extra = append(extra, m.anon...)
	}
	slices.Sort(extra)
	extra = slices.Compact(extra)
	key := p.identity().String() + "|" + joinRoles(extra)

	v, err := m.store.Version(ctx)
	if err != nil {
		return pol, err
	}
	m.mu.Lock()
	c, hit := m.cache[key]
	m.mu.Unlock()
	if !hit || c.version != v {
		c, err = m.load(ctx, p, extra)
		if err != nil {
			return pol, err
		}
		m.mu.Lock()
		if c.version != m.cacheVersion {
			// A new version invalidates every entry: drop them all, which
			// also keeps the cache bounded by the principals seen since.
			m.cache = map[string]cached{}
			m.cacheVersion = c.version
		}
		m.cache[key] = c
		m.mu.Unlock()
	}
	pol.Roles = slices.Clone(c.roles)
	pol.Version = c.version
	pol.perms = c.perms
	return pol, nil
}

func joinRoles(rs []Role) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = string(r)
	}
	return strings.Join(s, ",")
}

// load reads p's bindings and the role permissions in one read transaction,
// stamped with the version read inside it.
func (m *Materializer) load(ctx context.Context, p Principal, extra []Role) (cached, error) {
	tx, err := m.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cached{}, fmt.Errorf("authz: materialize %s: %w", p, err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	v, err := readVersion(ctx, tx)
	if err != nil {
		return cached{}, err
	}
	var roles []Role
	add := func(r Role) {
		if !admits(p, r) {
			if _, known := Seed[r]; !known {
				log.Warn().Str("principal", p.String()).Str("role", string(r)).Msg("authz: ignoring a binding to an unknown role")
			}
			return
		}
		if !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	err = scanAll(ctx, tx, `SELECT role FROM auth_role_bindings WHERE principal = ?`, func(r *sql.Rows) error {
		var role string
		if err := r.Scan(&role); err != nil {
			return err
		}
		add(Role(role))
		return nil
	}, p.identity().String())
	if err != nil {
		return cached{}, fmt.Errorf("authz: materialize %s: %w", p, err)
	}
	for _, r := range extra {
		add(r)
	}
	slices.Sort(roles)

	perms := map[Permission]bool{}
	if len(roles) > 0 {
		err = scanAll(ctx, tx, `SELECT role, permission FROM auth_role_permissions`, func(r *sql.Rows) error {
			var role, perm string
			if err := r.Scan(&role, &perm); err != nil {
				return err
			}
			if !slices.Contains(roles, Role(role)) {
				return nil
			}
			pp, perr := ParsePermission(perm)
			if perr != nil {
				log.Warn().Str("role", role).Str("permission", perm).Msg("authz: ignoring an unknown permission in the role table")
				return nil
			}
			perms[pp] = true
			return nil
		})
		if err != nil {
			return cached{}, fmt.Errorf("authz: materialize %s: %w", p, err)
		}
	}
	return cached{version: v, roles: roles, perms: perms}, nil
}
