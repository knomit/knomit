package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/edge/oauth"
	"knomit/internal/edge/web"
	"knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
)

// authCLIFixture serves the plain router with the P3 credential stores on a
// REAL local listener (socket, or pipe on Windows) with the kernel-credential
// ConnContext `knomit serve` uses: the CLI is judged by its OS identity.
type authCLIFixture struct {
	socket   string
	store    *oauth.Store
	bindings *authz.Store
}

func newAuthCLIFixture(t *testing.T) *authCLIFixture {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	grants := auth.NewSQLGrants(db)
	// What app.seedOwnPrincipal gives the server's own account at boot (the
	// old writeGate still runs in front of /auth until the flip).
	me, err := auth.LocalPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := grants.Grant(context.Background(), me, perm, "boot"); err != nil {
			t.Fatal(err)
		}
	}
	bindings := authz.NewStore(db)
	if err := bindings.SeedRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := oauth.NewStoreWith(db, oauth.DefaultLifetimes(), bindings)
	cfg := config.Defaults()
	cfg.ExternalAddresses = []string{"http://localhost:19278"}
	issuers, _ := cfg.Issuers()
	f := &authCLIFixture{socket: authLocalListenerPath(t), store: st, bindings: bindings}
	s := &web.Server{
		Auth:       config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault},
		Grants:     grants,
		AuthStore:  st,
		AuthIssuer: oauth.NewIssuer(oauth.Options{Store: st, Clients: oauth.NewResolver(nil), Grants: grants}),
		AuthzStore: bindings,
		Issuers:    issuers,
	}
	ln, cleanup, err := auth.ListenLocal(f.socket)
	if err != nil {
		t.Fatalf("ListenLocal: %v", err)
	}
	t.Cleanup(cleanup)
	srv := &http.Server{Handler: s.Handler(), ConnContext: auth.ConnContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func TestAuthLink_PrintsURL(t *testing.T) {
	f := newAuthCLIFixture(t)
	c := localAPIClient(f.socket)
	var out bytes.Buffer
	if err := authLink(context.Background(), c, &out, false, ""); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasPrefix(line, "http://localhost:19278/auth/link/") || strings.Contains(line, "\n") {
		t.Fatalf("output %q: want the bare URL on one line", out.String())
	}
	// The link signs in as THIS OS account, the local listener's peer.
	tok := strings.TrimPrefix(line, "http://localhost:19278/auth/link/")
	p, err := f.store.ConsumeLink(context.Background(), tok)
	me, _ := authz.Owner()
	if err != nil || p != me {
		t.Fatalf("link principal = %v, %v; want %v", p, err, me)
	}

	out.Reset()
	if err := authLink(context.Background(), c, &out, true, ""); err != nil {
		t.Fatal(err)
	}
	var j struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || !strings.Contains(j.URL, "/auth/link/") || j.ExpiresAt.IsZero() {
		t.Fatalf("--json: %q %v", out.String(), err)
	}
	if err := authLink(context.Background(), c, &out, false, "https://evil.example"); err == nil || !strings.Contains(err.Error(), "external_addresses") {
		t.Fatalf("--url unlisted: %v", err)
	}
}

func TestAuthTokensCreate_JSON(t *testing.T) {
	f := newAuthCLIFixture(t)
	ctx := context.Background()
	c := localAPIClient(f.socket)
	var out bytes.Buffer
	if err := authTokensCreate(ctx, c, &out, "operator", "e2e", "1h", true); err != nil {
		t.Fatal(err)
	}
	var j struct {
		ID, Secret, Role, Label string
		ExpiresAt               time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		t.Fatalf("--json %q: %v", out.String(), err)
	}
	if !strings.HasPrefix(j.Secret, oauth.PATPrefix) || j.Role != "operator" || j.Label != "e2e" || time.Until(j.ExpiresAt) > time.Hour+time.Minute {
		t.Fatalf("created = %+v", j)
	}
	pat, err := f.store.VerifyPAT(ctx, j.Secret)
	if err != nil || pat.ID != j.ID {
		t.Fatalf("the secret does not verify: %+v %v", pat, err)
	}

	// Plain output names the secret once and says it is shown once.
	out.Reset()
	if err := authTokensCreate(ctx, c, &out, "reader", "ci", "", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), oauth.PATPrefix) || !strings.Contains(out.String(), "shown once") {
		t.Fatalf("plain output: %s", out.String())
	}
	if err := authTokensCreate(ctx, c, &out, "admin", "x", "", false); err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("admin token: %v", err)
	}

	out.Reset()
	if err := authTokensList(ctx, c, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), j.ID) || !strings.Contains(out.String(), "e2e") || strings.Contains(out.String(), oauth.PATPrefix) {
		t.Fatalf("list: %s", out.String())
	}
	out.Reset()
	if err := authTokensRevoke(ctx, c, &out, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.VerifyPAT(ctx, j.Secret); err == nil {
		t.Fatal("revoked token still verifies")
	}
	if err := authTokensRevoke(ctx, c, &out, j.ID); err == nil {
		t.Fatal("second revoke succeeded")
	}
}

func TestAuthSessionsAndRoles(t *testing.T) {
	f := newAuthCLIFixture(t)
	ctx := context.Background()
	c := localAPIClient(f.socket)
	me, _ := authz.Owner()
	_, sess, err := f.store.CreateSession(ctx, me, "Firefox")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := authSessions(ctx, c, &out, false); err != nil || !strings.Contains(out.String(), sess.ID) || !strings.Contains(out.String(), "Firefox") {
		t.Fatalf("sessions: %v %s", err, out.String())
	}
	if err := authSessionsRevoke(ctx, c, &out, sess.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.store.ListSessions(ctx); len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}

	out.Reset()
	if err := authRolesBind(ctx, c, &out, "anonymous", "guest", "demo"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := authRolesList(ctx, c, &out, false); err != nil || !strings.Contains(out.String(), "anonymous") || !strings.Contains(out.String(), "guest") {
		t.Fatalf("roles list: %v %s", err, out.String())
	}
	if err := authRolesBind(ctx, c, &out, "anonymous", "admin", ""); err == nil {
		t.Fatal("anonymous bound to admin")
	}
	if err := authRolesUnbind(ctx, c, &out, "anonymous", "guest"); err != nil {
		t.Fatal(err)
	}
	if bs, _ := f.bindings.Bindings(ctx); len(bs) != 0 {
		t.Fatalf("still bound: %+v", bs)
	}
}

// The verbs are hidden until the flip (P4 unhides them) but registered.
func TestAuthCmd_RegisteredHidden(t *testing.T) {
	root := RootCmd()
	c, _, err := root.Find([]string{"auth"})
	if err != nil || c == nil || c.Name() != "auth" {
		t.Fatalf("auth not registered: %v", err)
	}
	if !c.Hidden {
		t.Fatal("knomit auth must stay hidden until the flip")
	}
	for _, path := range [][]string{
		{"auth", "link"}, {"auth", "tokens", "create"}, {"auth", "tokens", "list"}, {"auth", "tokens", "revoke"},
		{"auth", "sessions"}, {"auth", "sessions", "revoke"},
		{"auth", "roles", "list"}, {"auth", "roles", "bind"}, {"auth", "roles", "unbind"},
	} {
		sub, _, err := root.Find(path)
		if err != nil || sub.Name() != path[len(path)-1] {
			t.Errorf("%v: %v", path, err)
		}
	}
	// A typo under the group exits non-zero (gotchas/cli/cobra/group-command-typo-exits-zero).
	root.SetArgs([]string{"auth", "tokenz"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("`knomit auth tokenz` succeeded")
	}
}
