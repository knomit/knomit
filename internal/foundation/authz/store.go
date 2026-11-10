package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
)

// Binding binds one principal to one role, instance-wide.
type Binding struct {
	Principal Principal
	Role      Role
	Label     string    // display name of the principal ("kb on laptop")
	By        string    // who made the binding (a principal string, or "boot:…")
	At        time.Time // zero means now
}

// ErrTokenBound refuses a second binding for a token principal: a token is
// bound to exactly one role, at mint or approval (§13.2). Revoking the
// credential (UnbindAll) is the only way to free it.
var ErrTokenBound = errors.New("authz: a token is bound to exactly one role and this one already has its role")

// versionKey is auth_state's row for the bindings version.
const versionKey = "bindings_version"

// Store is the role seed, the bindings and the bindings version in
// control.db (migration 000022). Every change that can alter any principal's
// policy — a binding, an unbinding, a role seed that changed a row — bumps the
// version in the same transaction; the Materializer's cache is keyed on it.
type Store struct {
	db *sql.DB
}

// NewStore wraps the control.db handle. The tables must exist (migrate.Control).
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// bump increments the bindings version inside tx.
func bump(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO auth_state(key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`, versionKey)
	return err
}

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func readVersion(ctx context.Context, q queryer) (int64, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM auth_state WHERE key = ?`, versionKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("authz: read bindings version: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("authz: bindings version %q: %w", v, err)
	}
	return n, nil
}

// Version is the current bindings version: 0 before anything was ever seeded
// or bound. One SELECT.
func (s *Store) Version(ctx context.Context) (int64, error) { return readVersion(ctx, s.db) }

// SeedRoles makes auth_roles and auth_role_permissions equal Seed: it adds
// every missing row AND removes every row Seed does not hold (a role it does
// not name, or a permission a role no longer has), so a release that takes a
// permission away from a role takes it away from existing databases too. It is
// idempotent and bumps the version iff it changed a row. Boot calls it every
// time; it is Go and not a migration because control.db up-migrations may only
// CREATE ... IF NOT EXISTS.
func (s *Store) SeedRoles(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authz: seed roles: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	haveRoles := map[string]bool{}
	haveRows := map[[2]string]bool{}
	if err := scanAll(ctx, tx, `SELECT name FROM auth_roles`, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		haveRoles[n] = true
		return nil
	}); err != nil {
		return fmt.Errorf("authz: seed roles: %w", err)
	}
	if err := scanAll(ctx, tx, `SELECT role, permission FROM auth_role_permissions`, func(r *sql.Rows) error {
		var k [2]string
		if err := r.Scan(&k[0], &k[1]); err != nil {
			return err
		}
		haveRows[k] = true
		return nil
	}); err != nil {
		return fmt.Errorf("authz: seed roles: %w", err)
	}

	changed := false
	exec := func(q string, args ...any) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("authz: seed roles: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
		}
		return nil
	}
	for role, perms := range Seed {
		if !haveRoles[string(role)] {
			if err := exec(`INSERT INTO auth_roles(name) VALUES (?)`, string(role)); err != nil {
				return err
			}
		}
		for _, p := range perms {
			if !haveRows[[2]string{string(role), string(p)}] {
				if err := exec(`INSERT INTO auth_role_permissions(role, permission) VALUES (?, ?)`, string(role), string(p)); err != nil {
					return err
				}
			}
		}
	}
	for k := range haveRows {
		if !slices.Contains(Seed[Role(k[0])], Permission(k[1])) {
			if err := exec(`DELETE FROM auth_role_permissions WHERE role = ? AND permission = ?`, k[0], k[1]); err != nil {
				return err
			}
		}
	}
	for n := range haveRoles {
		if _, ok := Seed[Role(n)]; !ok {
			if err := exec(`DELETE FROM auth_roles WHERE name = ?`, n); err != nil {
				return err
			}
		}
	}
	if changed {
		if err := bump(ctx, tx); err != nil {
			return fmt.Errorf("authz: seed roles: %w", err)
		}
	}
	return tx.Commit()
}

func scanAll(ctx context.Context, q queryer, query string, each func(*sql.Rows) error, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// checkBindable applies the binding rules that need no database.
func checkBindable(b Binding) error {
	p := b.Principal.identity()
	if !canonical(p) {
		return fmt.Errorf("authz: %q is not a principal in its canonical spelling", p.String())
	}
	if p.Kind == KindNode {
		return errors.New("authz: a node principal is never bound: its only role is the node role its instance certificate carries (§13.1)")
	}
	if _, ok := Seed[b.Role]; !ok {
		return fmt.Errorf("authz: unknown role %q", b.Role)
	}
	if b.By == "" {
		return errors.New("authz: a binding must record who made it (By)")
	}
	owner, err := Owner()
	if err != nil {
		return err
	}
	if p == owner {
		return errors.New("authz: the owner is implicit and total: it is never bound to a role")
	}
	if b.Role == RoleNode {
		return errors.New("authz: the node role is never bound: a fleet node holds it through its instance certificate (§13.1)")
	}
	switch p.Kind {
	case KindAnonymous:
		if b.Role != RoleGuest {
			return fmt.Errorf("authz: anonymous may be bound only to guest, not %s (§15.3)", b.Role)
		}
	case KindToken:
		if !slices.Contains(tokenRoles, b.Role) {
			return fmt.Errorf("authz: a token may be bound only to one of %v, not %s", tokenRoles, b.Role)
		}
	}
	return nil
}

// Bind binds b.Principal to b.Role. It refuses the owner (implicit), the node
// role (certificate-only), anonymous to anything but guest, a token to
// anything but TokenRoles, and a token that already has a role
// (ErrTokenBound). Binding an OS principal to a role it already holds is a
// no-op. A change bumps the version.
func (s *Store) Bind(ctx context.Context, b Binding) error {
	if err := checkBindable(b); err != nil {
		return err
	}
	p := b.Principal.identity()
	at := b.At
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authz: bind: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	if p.Kind == KindToken {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_role_bindings WHERE principal = ?`, p.String()).Scan(&n); err != nil {
			return fmt.Errorf("authz: bind: %w", err)
		}
		if n > 0 {
			return ErrTokenBound
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO auth_role_bindings(principal, role, label, created_by, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(principal, role) DO NOTHING`,
		p.String(), string(b.Role), b.Label, b.By, at.Unix())
	if err != nil {
		return fmt.Errorf("authz: bind: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := bump(ctx, tx); err != nil {
			return fmt.Errorf("authz: bind: %w", err)
		}
	}
	return tx.Commit()
}

// unbind deletes the rows matching where and bumps the version iff any went.
func (s *Store) unbind(ctx context.Context, where string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("authz: unbind: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `DELETE FROM auth_role_bindings WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("authz: unbind: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := bump(ctx, tx); err != nil {
			return fmt.Errorf("authz: unbind: %w", err)
		}
	}
	return tx.Commit()
}

// Unbind removes one binding. Removing one that does not exist is a no-op.
func (s *Store) Unbind(ctx context.Context, p Principal, r Role) error {
	return s.unbind(ctx, `principal = ? AND role = ?`, p.identity().String(), string(r))
}

// UnbindAll removes every binding of p: what revoking a token does.
func (s *Store) UnbindAll(ctx context.Context, p Principal) error {
	return s.unbind(ctx, `principal = ?`, p.identity().String())
}

// Bindings lists every stored binding, by principal then role. A row whose
// principal does not parse is skipped with a WARN (it can only come from a
// hand edit, and it binds nobody).
func (s *Store) Bindings(ctx context.Context) ([]Binding, error) {
	var out []Binding
	err := scanAll(ctx, s.db, `SELECT principal, role, label, created_by, created_at
		FROM auth_role_bindings ORDER BY principal, role`, func(r *sql.Rows) error {
		var ps, role, label, by string
		var at int64
		if err := r.Scan(&ps, &role, &label, &by, &at); err != nil {
			return err
		}
		p, err := ParsePrincipal(ps)
		if err != nil {
			log.Warn().Str("principal", ps).Err(err).Msg("authz: skipping a binding whose principal does not parse")
			return nil
		}
		p.Label = label
		out = append(out, Binding{Principal: p, Role: Role(role), Label: label, By: by, At: time.Unix(at, 0)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("authz: list bindings: %w", err)
	}
	return out, nil
}

func markerKey(name string) string { return "marker:" + name }

// Marker reports whether the one-shot boot step name has run.
func (s *Store) Marker(ctx context.Context, name string) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM auth_state WHERE key = ?`, markerKey(name)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("authz: read marker %q: %w", name, err)
	}
	return true, nil
}

// SetMarker records that the one-shot boot step name has run. Idempotent; it
// is not a binding change and does not move the version.
func (s *Store) SetMarker(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_state(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`,
		markerKey(name), strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		return fmt.Errorf("authz: set marker %q: %w", name, err)
	}
	return nil
}
