package authz

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func perms(p Policy) []Permission { return p.Permissions() }

func TestMaterialize_OwnerHasEverything(t *testing.T) {
	s := seededStore(t)
	owner, err := Owner()
	if err != nil {
		t.Fatal(err)
	}
	m := NewMaterializer(s, owner, MaterializerOptions{})
	p, err := m.For(context.Background(), owner, ViaSocket)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Owner {
		t.Fatal("the owner's policy is not marked Owner")
	}
	for _, perm := range append(All(), Public) {
		if !p.Has(perm) {
			t.Fatalf("owner lacks %s", perm)
		}
	}
	if got := perms(p); !slices.Equal(got, All()) {
		t.Fatalf("owner Permissions() = %v, want all ten", got)
	}
	if bs, _ := s.Bindings(context.Background()); len(bs) != 0 {
		t.Fatal("the owner needs no binding")
	}
	// A label does not make the owner somebody else.
	labelled := owner
	labelled.Label = "me"
	if p, _ := m.For(context.Background(), labelled, ViaSession); !p.Owner {
		t.Fatal("a labelled owner lost the owner policy")
	}
}

func TestMaterialize_UnionForOSPrincipal(t *testing.T) {
	ctx := context.Background()
	s := seededStore(t)
	os := Principal{Kind: KindOS, ID: "uid:4242"}
	if err := bind(s, os, RoleReader); err != nil {
		t.Fatal(err)
	}
	if err := bind(s, os, RoleOperator); err != nil {
		t.Fatal(err)
	}
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{})
	p, err := m.For(ctx, os, ViaSocket)
	if err != nil {
		t.Fatal(err)
	}
	want := []Permission{KBRead, KBWrite, ChatUse, RepoManage, RepoOrigin, OpsRun}
	if got := perms(p); !slices.Equal(got, want) {
		t.Fatalf("reader+operator = %v, want %v", got, want)
	}
	if p.Owner || p.Has(SecretManage) || p.Has(RepoPush) {
		t.Fatalf("union grants more than its roles: %+v %v", p, perms(p))
	}
	if got := p.Roles; !slices.Equal(got, []Role{RoleOperator, RoleReader}) {
		t.Fatalf("Roles = %v", got)
	}
	if p.Via != ViaSocket || p.Principal != os {
		t.Fatalf("policy identity = %+v", p)
	}
}

// An owner that could not be resolved must match nobody — least of all a
// zero principal a careless caller passes.
func TestMaterialize_ZeroOwnerMatchesNobody(t *testing.T) {
	m := NewMaterializer(seededStore(t), Principal{}, MaterializerOptions{})
	for _, pr := range []Principal{{}, {Kind: KindOS}} {
		p, err := m.For(context.Background(), pr, ViaNone)
		if err != nil {
			t.Fatal(err)
		}
		if p.Owner || p.Has(KBRead) {
			t.Fatalf("%+v matched a zero owner", pr)
		}
	}
}

func TestMaterialize_UnboundPrincipalHasNothing(t *testing.T) {
	s := seededStore(t)
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{})
	for _, pr := range []Principal{{Kind: KindOS, ID: "uid:0"}, {Kind: KindToken, ID: "t"}, Anonymous()} {
		p, err := m.For(context.Background(), pr, ViaNone)
		if err != nil {
			t.Fatal(err)
		}
		if len(perms(p)) != 0 || p.Has(KBRead) {
			t.Fatalf("%s with no binding has %v (root included: other accounts get nothing until bound)", pr, perms(p))
		}
		if !p.Has(Public) {
			t.Fatal("public routes need no permission")
		}
	}
}

func TestMaterialize_ReadOnlyAnonymousGuestInMemory(t *testing.T) {
	ctx := context.Background()
	s := seededStore(t)
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{AnonymousRoles: []Role{RoleGuest}})
	p, err := m.For(ctx, Anonymous(), ViaNone)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Has(KBRead) {
		t.Fatal("read-only anonymous lacks kb:read")
	}
	if p.Has(ChatUse) || p.Has(KBWrite) {
		t.Fatalf("read-only anonymous holds more than guest: %v", perms(p))
	}
	if bs, _ := s.Bindings(ctx); len(bs) != 0 {
		t.Fatalf("the in-memory guest role was stored: %v", bs)
	}
	// Only anonymous gets the in-memory roles.
	if p, _ := m.For(ctx, Principal{Kind: KindToken, ID: "x"}, ViaBearer); p.Has(KBRead) {
		t.Fatal("AnonymousRoles leaked to a token principal")
	}
	// A stored anonymous guest binding unions with it (same role: no change).
	if err := bind(s, Anonymous(), RoleGuest); err != nil {
		t.Fatal(err)
	}
	p, err = m.For(ctx, Anonymous(), ViaNone)
	if err != nil {
		t.Fatal(err)
	}
	if got := perms(p); !slices.Equal(got, []Permission{KBRead}) || !slices.Equal(p.Roles, []Role{RoleGuest}) {
		t.Fatalf("stored+in-memory guest = %v roles %v", got, p.Roles)
	}
}

// §15.3: anonymous may hold guest only, whatever the options say and
// whatever a hand-edited row says.
func TestMaterialize_AnonymousNeverMoreThanGuest(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewStore(db)
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_role_bindings(principal, role, created_by, created_at) VALUES ('anonymous', 'admin', 'hand', 0)`); err != nil {
		t.Fatal(err)
	}
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{AnonymousRoles: []Role{RoleReader, RoleGuest}})
	p, err := m.For(ctx, Anonymous(), ViaNone)
	if err != nil {
		t.Fatal(err)
	}
	if got := perms(p); !slices.Equal(got, []Permission{KBRead}) {
		t.Fatalf("anonymous = %v, want guest's kb:read only", got)
	}
}

func TestMaterialize_ImplicitNodeRole(t *testing.T) {
	s := seededStore(t)
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{})
	node := Principal{Kind: KindNode, ID: "9f86d081884c7d65"}
	p, err := m.For(context.Background(), node, ViaMTLS, RoleNode)
	if err != nil {
		t.Fatal(err)
	}
	if got := perms(p); !slices.Equal(got, []Permission{KBRead, RepoPush}) {
		t.Fatalf("node = %v, want kb:read + repo:push", got)
	}
	// The implicit node role means something only for a node principal: an
	// OS account or token asking for it gets nothing from it.
	for _, other := range []Principal{{Kind: KindOS, ID: "uid:7"}, {Kind: KindToken, ID: "t"}} {
		p, err := m.For(context.Background(), other, ViaBearer, RoleNode)
		if err != nil {
			t.Fatal(err)
		}
		if p.Has(RepoPush) {
			t.Fatalf("%s got repo:push from an implicit node role", other)
		}
	}
	// And the cache must not hand one call's implicit roles to another.
	p, err = m.For(context.Background(), node, ViaMTLS)
	if err != nil {
		t.Fatal(err)
	}
	if p.Has(RepoPush) {
		t.Fatal("the cached implicit role leaked into a call that did not ask for it")
	}
}

func TestMaterialize_CacheInvalidatesOnBind(t *testing.T) {
	ctx := context.Background()
	s := seededStore(t)
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{})
	tok := Principal{Kind: KindToken, ID: "t9"}
	p1, err := m.For(ctx, tok, ViaBearer)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Has(KBRead) {
		t.Fatal("unbound token reads")
	}
	if err := bind(s, tok, RoleContributor); err != nil {
		t.Fatal(err)
	}
	p2, err := m.For(ctx, tok, ViaBearer)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Has(KBWrite) || p2.Version <= p1.Version {
		t.Fatalf("after Bind: perms %v version %d -> %d", perms(p2), p1.Version, p2.Version)
	}
	if err := s.UnbindAll(ctx, tok); err != nil {
		t.Fatal(err)
	}
	p3, err := m.For(ctx, tok, ViaBearer)
	if err != nil {
		t.Fatal(err)
	}
	if p3.Has(KBRead) {
		t.Fatal("a revoked token kept its cached policy")
	}
	// A reseed that changes a role's permissions invalidates too.
	if err := bind(s, tok, RoleReader); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.For(ctx, tok, ViaBearer); !p.Has(ChatUse) {
		t.Fatal("reader lacks chat:use")
	}
	if _, err := s.db.Exec(`DELETE FROM auth_role_permissions WHERE role='reader' AND permission='chat:use'`); err != nil {
		t.Fatal(err)
	}
	// A hand edit does not move the version, so the cache still answers…
	if p, _ := m.For(ctx, tok, ViaBearer); !p.Has(ChatUse) {
		t.Fatal("expected the cached answer until the version moves")
	}
	// …and the reseed (which restores the row) moves it.
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.For(ctx, tok, ViaBearer); !p.Has(ChatUse) || p.Version <= p2.Version {
		t.Fatalf("after reseed: %v v%d", perms(p), p.Version)
	}
}

func TestMaterialize_StoreErrorDenies(t *testing.T) {
	db := testDB(t)
	s := NewStore(db)
	if err := s.SeedRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	os := Principal{Kind: KindOS, ID: "uid:4242"}
	if err := bind(s, os, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{AnonymousRoles: []Role{RoleGuest}})
	// Warm the cache: a store error must deny even what was cached.
	if p, err := m.For(context.Background(), os, ViaSocket); err != nil || !p.Has(AuthManage) {
		t.Fatalf("warm-up: %v %v", perms(p), err)
	}
	db.Close()
	for _, pr := range []Principal{os, Anonymous()} {
		p, err := m.For(context.Background(), pr, ViaSocket)
		if err == nil {
			t.Fatalf("%s: no error from a closed store", pr)
		}
		if len(perms(p)) != 0 || p.Has(KBRead) || p.Owner {
			t.Fatalf("%s: a store error granted %v", pr, perms(p))
		}
	}
}

func TestMaterialize_UnknownPermissionRowIgnored(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewStore(db)
	if err := s.SeedRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_role_permissions(role, permission) VALUES ('reader', 'x:y')`); err != nil {
		t.Fatal(err)
	}
	os := Principal{Kind: KindOS, ID: "uid:4242"}
	if err := bind(s, os, RoleReader); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })

	m := NewMaterializer(s, Principal{Kind: KindOS, ID: "uid:1"}, MaterializerOptions{})
	p, err := m.For(ctx, os, ViaSocket)
	if err != nil {
		t.Fatal(err)
	}
	if got := perms(p); !slices.Equal(got, []Permission{KBRead, ChatUse}) {
		t.Fatalf("reader with an x:y row = %v", got)
	}
	if p.Has("x:y") {
		t.Fatal("an unknown permission was honoured")
	}
	if out := buf.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "x:y") {
		t.Fatalf("no WARN naming the unknown permission; log = %q", out)
	}
}

func TestPolicy_Context(t *testing.T) {
	if _, ok := PolicyFrom(context.Background()); ok {
		t.Fatal("an empty context carries a policy")
	}
	var zero Policy
	if zero.Has(KBRead) || !zero.Has(Public) {
		t.Fatal("the zero policy must deny everything but public")
	}
	p := Policy{Principal: Anonymous(), Via: ViaNone}
	got, ok := PolicyFrom(WithPolicy(context.Background(), p))
	if !ok || got.Principal != Anonymous() {
		t.Fatalf("round trip = %+v %v", got, ok)
	}
}
