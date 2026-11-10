package authz

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	storemigrate "knomit/internal/engine/store/migrate"
)

// testDB is a migrated temp control.db with the DSN the repo registry uses.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // same as the real registry handle
	t.Cleanup(func() { db.Close() })
	if err := storemigrate.Control(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seededStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(testDB(t))
	if err := s.SeedRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func version(t *testing.T, s *Store) int64 {
	t.Helper()
	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rolePerms(t *testing.T, db *sql.DB, role Role) []Permission {
	t.Helper()
	rows, err := db.Query(`SELECT permission FROM auth_role_permissions WHERE role = ? ORDER BY permission`, string(role))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Permission
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, Permission(p))
	}
	return out
}

func TestSeedRoles_IdempotentAndVersioned(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewStore(db)
	if v := version(t, s); v != 0 {
		t.Fatalf("fresh version = %d, want 0", v)
	}
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	v1 := version(t, s)
	if v1 == 0 {
		t.Fatal("the first seed changed the tables and must bump the version")
	}
	for role, perms := range Seed {
		want := slices.Clone(perms)
		slices.Sort(want)
		if got := rolePerms(t, db, role); !slices.Equal(got, want) {
			t.Fatalf("after seed, %s = %v, want %v", role, got, want)
		}
	}

	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if v := version(t, s); v != v1 {
		t.Fatalf("a no-op seed moved the version %d -> %d: every cached policy would be dropped at each boot for nothing", v1, v)
	}

	if _, err := db.Exec(`DELETE FROM auth_role_permissions WHERE role = 'reader' AND permission = 'chat:use'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rolePerms(t, db, RoleReader); !slices.Contains(got, ChatUse) {
		t.Fatalf("the deleted row did not come back: reader = %v", got)
	}
	if v := version(t, s); v != v1+1 {
		t.Fatalf("version = %d, want %d after a seed that changed one row", v, v1+1)
	}
}

// Seed is the authority in BOTH directions: a release that takes a permission
// away from a role (as §16 took repo:push away from contributor) must take it
// away from databases seeded by an older release, not only from fresh ones.
func TestSeedRoles_RemovesWhatSeedDoesNotHold(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewStore(db)
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	v1 := version(t, s)
	for _, q := range []string{
		`INSERT INTO auth_role_permissions(role, permission) VALUES ('contributor', 'repo:push')`,
		`INSERT INTO auth_roles(name) VALUES ('superuser')`,
		`INSERT INTO auth_role_permissions(role, permission) VALUES ('superuser', 'auth:manage')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rolePerms(t, db, RoleContributor); slices.Contains(got, RepoPush) {
		t.Fatalf("contributor kept repo:push after reseed: %v", got)
	}
	if got := rolePerms(t, db, "superuser"); len(got) != 0 {
		t.Fatalf("a role not in Seed kept its permissions: %v", got)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_roles WHERE name = 'superuser'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a role not in Seed survived the seed (n=%d, err=%v)", n, err)
	}
	if v := version(t, s); v != v1+1 {
		t.Fatalf("version = %d, want %d", v, v1+1)
	}
}

func bind(s *Store, p Principal, r Role) error {
	return s.Bind(context.Background(), Binding{Principal: p, Role: r, By: "test"})
}

func TestBind_RefusesOwner(t *testing.T) {
	s := seededStore(t)
	owner, err := Owner()
	if err != nil {
		t.Fatal(err)
	}
	for r := range Seed {
		if err := bind(s, owner, r); err == nil {
			t.Fatalf("bound the owner to %s: the owner is implicit and total, never bound", r)
		}
	}
	// A label is not identity: labelling the owner does not make it bindable.
	labelled := owner
	labelled.Label = "me"
	if err := bind(s, labelled, RoleReader); err == nil {
		t.Fatal("bound the owner under a label")
	}
	if bs, _ := s.Bindings(context.Background()); len(bs) != 0 {
		t.Fatalf("refused binds left rows: %v", bs)
	}
}

func TestBind_TokenOnlyOneRoleNeverAdmin(t *testing.T) {
	s := seededStore(t)
	tok := Principal{Kind: KindToken, ID: "t1"}
	for _, r := range []Role{RoleAdmin, RoleGuest, RoleNode} {
		if err := bind(s, tok, r); err == nil {
			t.Fatalf("bound a token to %s", r)
		}
	}
	for _, r := range TokenRoles() {
		other := Principal{Kind: KindToken, ID: "t-" + string(r)}
		if err := bind(s, other, r); err != nil {
			t.Fatalf("token -> %s refused: %v", r, err)
		}
	}
	if err := bind(s, tok, RoleReader); err != nil {
		t.Fatal(err)
	}
	v := version(t, s)
	for _, r := range TokenRoles() {
		err := bind(s, tok, r)
		if err == nil {
			t.Fatalf("a token bound to reader was also bound to %s: a token holds exactly one role", r)
		}
		if !errors.Is(err, ErrTokenBound) {
			t.Fatalf("second token bind: %v, want ErrTokenBound", err)
		}
	}
	if version(t, s) != v {
		t.Fatal("a refused bind moved the version")
	}
	if err := s.UnbindAll(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if err := bind(s, tok, RoleOperator); err != nil {
		t.Fatalf("after UnbindAll the token is a fresh principal again: %v", err)
	}
}

func TestBind_AnonymousGuestOnly(t *testing.T) {
	s := seededStore(t)
	for r := range Seed {
		if r == RoleGuest {
			continue
		}
		if err := bind(s, Anonymous(), r); err == nil {
			t.Fatalf("bound anonymous to %s: §15.3 allows guest only", r)
		}
	}
	if err := bind(s, Anonymous(), RoleGuest); err != nil {
		t.Fatalf("anonymous -> guest refused: %v", err)
	}
}

// node is implicit (§13.1): it comes from the certificate, so nobody is bound
// to it — not an OS account, not a node principal.
func TestBind_NodeRoleIsNeverBound(t *testing.T) {
	s := seededStore(t)
	for _, p := range []Principal{
		{Kind: KindOS, ID: "uid:4242"},
		{Kind: KindNode, ID: "abcd"},
	} {
		if err := bind(s, p, RoleNode); err == nil {
			t.Fatalf("bound %s to node", p)
		}
	}
}

// A node principal is bound to nothing at all: its only role is the implicit
// node role its certificate carries.
func TestBind_NodePrincipalIsNeverBound(t *testing.T) {
	s := seededStore(t)
	node := Principal{Kind: KindNode, ID: "9f86d081884c7d65"}
	for r := range Seed {
		if err := bind(s, node, r); err == nil {
			t.Fatalf("bound node principal to %s", r)
		}
	}
	if bs, _ := s.Bindings(context.Background()); len(bs) != 0 {
		t.Fatalf("refused binds left rows: %v", bs)
	}
}

func TestBind_ValidatesInput(t *testing.T) {
	s := seededStore(t)
	os := Principal{Kind: KindOS, ID: "uid:4242"}
	if err := bind(s, os, "superuser"); err == nil {
		t.Fatal("bound an unknown role")
	}
	if err := bind(s, Principal{Kind: "bridge", ID: "uid:1"}, RoleReader); err == nil {
		t.Fatal("bound a malformed principal")
	}
	if err := s.Bind(context.Background(), Binding{Principal: os, Role: RoleReader}); err == nil {
		t.Fatal("bound with no By: every binding records who made it")
	}
}

func TestBind_UnbindAndBindingsAndVersion(t *testing.T) {
	ctx := context.Background()
	s := seededStore(t)
	os := Principal{Kind: KindOS, ID: "uid:4242"}
	v0 := version(t, s)
	at := time.Unix(1_800_000_000, 0)
	if err := s.Bind(ctx, Binding{Principal: os, Role: RoleReader, Label: "ci user", By: "os:uid:1000", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := bind(s, os, RoleOperator); err != nil {
		t.Fatal(err)
	}
	if v := version(t, s); v != v0+2 {
		t.Fatalf("version = %d, want %d after two binds", v, v0+2)
	}
	// Binding what is already bound changes nothing and moves nothing.
	if err := bind(s, os, RoleReader); err != nil {
		t.Fatalf("re-binding an OS principal's existing role: %v", err)
	}
	if v := version(t, s); v != v0+2 {
		t.Fatalf("a no-op bind moved the version to %d", v)
	}
	bs, err := s.Bindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("Bindings = %v, want 2", bs)
	}
	var reader Binding
	for _, b := range bs {
		if b.Role == RoleReader {
			reader = b
		}
	}
	if reader.Principal.identity() != os || reader.Principal.Label != "ci user" || reader.Label != "ci user" || reader.By != "os:uid:1000" || !reader.At.Equal(at) {
		t.Fatalf("reader binding read back as %+v", reader)
	}

	if err := s.Unbind(ctx, os, RoleReader); err != nil {
		t.Fatal(err)
	}
	if v := version(t, s); v != v0+3 {
		t.Fatalf("version = %d after unbind, want %d", v, v0+3)
	}
	if err := s.Unbind(ctx, os, RoleReader); err != nil {
		t.Fatalf("unbinding what is not bound: %v", err)
	}
	if v := version(t, s); v != v0+3 {
		t.Fatal("a no-op unbind moved the version")
	}
	if err := s.UnbindAll(ctx, os); err != nil {
		t.Fatal(err)
	}
	if bs, _ := s.Bindings(ctx); len(bs) != 0 {
		t.Fatalf("UnbindAll left %v", bs)
	}
}

func TestMarker_OneShot(t *testing.T) {
	ctx := context.Background()
	s := seededStore(t)
	set, err := s.Marker(ctx, "wipe-old-auth")
	if err != nil || set {
		t.Fatalf("fresh marker = %v, %v", set, err)
	}
	if err := s.SetMarker(ctx, "wipe-old-auth"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMarker(ctx, "wipe-old-auth"); err != nil {
		t.Fatalf("setting a set marker again: %v", err)
	}
	if set, err := s.Marker(ctx, "wipe-old-auth"); err != nil || !set {
		t.Fatalf("marker after set = %v, %v", set, err)
	}
	if set, _ := s.Marker(ctx, "other"); set {
		t.Fatal("markers are per name")
	}
	// A marker is not a binding change.
	v := version(t, s)
	_ = s.SetMarker(ctx, "third")
	if version(t, s) != v {
		t.Fatal("SetMarker moved the bindings version")
	}
}
