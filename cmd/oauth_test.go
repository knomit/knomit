package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/store/migrate"
	"knomit/internal/web"
)

// oauthCLIFixture serves the plain router on a REAL local listener with the
// kernel-credential ConnContext `knomit serve` uses, so the CLI below is
// judged exactly as in production: by its OS identity, over the socket (or
// pipe), with nothing presented.
type oauthCLIFixture struct {
	socket string
	iss    *oauth.Issuer
	grants *auth.SQLGrants
}

func newOAuthCLIFixture(t *testing.T, extra ...config.OAuthClient) *oauthCLIFixture {
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
	f := &oauthCLIFixture{socket: oauthLocalListenerPath(t), grants: auth.NewSQLGrants(db)}
	// What app.seedOwnPrincipal gives the server's own account at boot.
	me, err := auth.LocalPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), me, perm, "boot"); err != nil {
			t.Fatal(err)
		}
	}
	store := oauth.NewStore(db, 2*time.Hour, 14*24*time.Hour)
	oc := config.Defaults().OAuth
	oc.Clients = extra
	f.iss = oauth.NewIssuer(oauth.Options{
		Issuer: "http://127.0.0.1:19280", Store: store,
		Clients: oauth.NewResolver(oc.EffectiveClients()), Grants: f.grants,
	})
	s := &web.Server{
		// No repo manager: the approval endpoints touch none.
		Auth:           config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault},
		Grants:         f.grants,
		OAuthIssuer:    f.iss,
		BearerVerifier: oauth.NewVerifier("http://127.0.0.1:19280", store),
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

// park runs a real /oauth/authorize and returns the pending id.
func (f *oauthCLIFixture) park(t *testing.T, scope string) string {
	t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {"kb"}, "redirect_uri": {"http://127.0.0.1:5555/callback"},
		"code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"},
		"scope": {scope}, "resource": {"http://127.0.0.1:19280"},
	}
	ids := func() map[string]bool {
		list, err := f.iss.Pending(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, p := range list {
			out[p.ID] = true
		}
		return out
	}
	before := ids()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "cli-test-browser")
	rec := httptest.NewRecorder()
	f.iss.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	// Rows created in the same second sort by their random id: find the NEW one.
	for id := range ids() {
		if !before[id] {
			return id
		}
	}
	t.Fatal("nothing new pending")
	return ""
}

func TestOAuthCLI_PendingApproveDenyOverTheLocalListener(t *testing.T) {
	f := newOAuthCLIFixture(t)
	ctx := context.Background()
	c := localAPIClient(f.socket)
	a := f.park(t, "read write")
	b := f.park(t, "read")

	var out bytes.Buffer
	if err := oauthPending(ctx, c, &out); err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, want := range []string{a, b, "kb (knomit bridge)", "cli-test-browser", "http://127.0.0.1:5555/callback", "read write"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("pending output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := oauthApprove(ctx, c, &out, a, "laptop", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out.String(), "host:laptop@token") || !strings.Contains(out.String(), "read write") {
		t.Fatalf("approve output: %s", out.String())
	}
	me, _ := auth.LocalPrincipal()
	rows, _ := f.grants.List(ctx, "host:laptop@token")
	if len(rows) != 2 || rows[0].GrantedBy != me.String() {
		t.Fatalf("grants must be written by the LOCAL principal %s: %+v", me, rows)
	}

	out.Reset()
	if err := oauthDeny(ctx, c, &out, b); err != nil {
		t.Fatalf("deny: %v", err)
	}
	out.Reset()
	if err := oauthPending(ctx, c, &out); err != nil || strings.Contains(out.String(), a) || strings.Contains(out.String(), b) {
		t.Fatalf("after deciding both: %v\n%s", err, out.String())
	}
}

func TestOAuthCLI_ScopesAndErrors(t *testing.T) {
	f := newOAuthCLIFixture(t)
	ctx := context.Background()
	c := localAPIClient(f.socket)
	id := f.park(t, "read write")
	var out bytes.Buffer
	if err := oauthApprove(ctx, c, &out, id, "laptop", []string{"admin"}); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("--scopes admin: %v", err)
	}
	if err := oauthApprove(ctx, c, &out, "no-such-id", "laptop", nil); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Fatalf("unknown id: %v", err)
	}
	if err := oauthApprove(ctx, c, &out, id, "laptop", []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `ceiling "read"`) { // quoted since the 3b review (B1)
		t.Fatalf("--scopes read: %s", out.String())
	}
}

// A re-approval leaves the subject's grants as the operator left them (F19
// 3c R5), and the CLI says so with the way to widen — otherwise an operator
// who approved --scopes read,write would be left guessing why write fails.
func TestOAuthCLI_ReapprovalNamesTheWayToWiden(t *testing.T) {
	f := newOAuthCLIFixture(t)
	ctx := context.Background()
	c := localAPIClient(f.socket)
	var out bytes.Buffer
	if err := oauthApprove(ctx, c, &out, f.park(t, "read"), "laptop", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "grants unchanged") {
		t.Fatalf("a first approval writes grants: %s", out.String())
	}
	out.Reset()
	if err := oauthApprove(ctx, c, &out, f.park(t, "read write"), "laptop", []string{"read", "write"}); err != nil {
		t.Fatal(err)
	}
	want := "grants unchanged; widen with `knomit grants add \"host:laptop@token\" <perm>`"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("re-approval output lacks %q:\n%s", want, out.String())
	}
}

// Nothing listening: the error says to start the server, not a raw dial error.
func TestOAuthCLI_NoServer(t *testing.T) {
	c := localAPIClient(oauthLocalListenerPath(t))
	err := oauthPending(context.Background(), c, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "knomit serve") {
		t.Fatalf("no server: %v", err)
	}
}

// Whatever reaches the pending list, `knomit oauth pending` prints it inert:
// a hostile client name (here a pre-registered one, which CIMD ingest never
// checks) and a user agent with a bidi override come out escaped, and no raw
// ESC or U+202E reaches the terminal (review B2).
func TestOAuthCLI_PendingDisplayIsInert(t *testing.T) {
	f := newOAuthCLIFixture(t, config.OAuthClient{ID: "evil", Name: "Claude Code\x1b[8m", RedirectURIs: []string{"http://127.0.0.1/cb"}})
	q := url.Values{
		"response_type": {"code"}, "client_id": {"evil"}, "redirect_uri": {"http://127.0.0.1:5555/cb"},
		"code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"},
		"scope": {"read"}, "resource": {"http://127.0.0.1:19280"},
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "agent\u202eevil")
	rec := httptest.NewRecorder()
	f.iss.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	var out bytes.Buffer
	if err := oauthPending(context.Background(), localAPIClient(f.socket), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x202e) {
		t.Fatalf("raw control/format character reached the terminal:\n%q", got)
	}
	if !strings.Contains(got, `\x1b[8m`) || !strings.Contains(got, `\u202e`) {
		t.Fatalf("hostile fields not shown escaped:\n%s", got)
	}
}
