package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki/pkitest"
)

// signIn parks a request, approves it over the local listener, and completes
// it the way the browser and client would (collect, exchange the code), so
// the family is created by the production path. It returns the access token.
func (f *oauthWebFixture) signIn(t *testing.T, subject string) string {
	t.Helper()
	ctx := context.Background()
	id := f.park(t)
	req := localPeer(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/approve",
		strings.NewReader(`{"subject":"`+subject+`","scopes":["read"]}`)))
	req.Header.Set("Content-Type", "application/json")
	if rec := serve(f.s.Handler(), req); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	code, _, err := f.store.Collect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cd, err := f.store.ConsumeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := f.store.IssuePair(ctx, cd.Family.ID)
	if err != nil {
		t.Fatal(err)
	}
	return iss.Access
}

type grantsBody struct {
	Grants []map[string]any `json:"grants"`
}

func listGrants(t *testing.T, h http.Handler) grantsBody {
	t.Helper()
	rec := serve(h, localPeer(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/grants", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var out grantsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list body %s: %v", rec.Body.String(), err)
	}
	return out
}

// The local operator lists the approved logins with what the Access page
// shows — the client's name from its request, who approved it, when it last
// got a token — and revoking one signs its token out on the OAuth listener at
// once while another login keeps working.
func TestGrantsAPI_LocalListsAndRevokes(t *testing.T) {
	f := newKBFixture(t)
	h := f.s.Handler()
	tok := f.signIn(t, "laptop")
	other := f.signIn(t, "desktop")
	bearer := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
		return serve(withAuthorization(f.s.OAuthHandler(), "Bearer "+token), req).Code
	}
	if c := bearer(tok); c != http.StatusOK {
		t.Fatalf("token before revoke: %d", c)
	}

	list := listGrants(t, h)
	if len(list.Grants) != 2 {
		t.Fatalf("grants = %+v", list.Grants)
	}
	var row map[string]any
	for _, g := range list.Grants {
		if g["subject"] == "laptop" {
			row = g
		}
	}
	if row == nil {
		t.Fatalf("no laptop login: %+v", list.Grants)
	}
	for _, k := range []string{"id", "client_id", "client_name", "subject", "scopes", "resource", "approved_by", "created_at", "last_used_at", "refresh_expires_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("grant row lacks %q: %v", k, row)
		}
	}
	if row["client_id"] != "kb" || row["client_name"] == "" || row["last_used_at"] == nil {
		t.Fatalf("row = %v", row)
	}
	if row["approved_by"] != "bridge:uid:501@"+string(auth.LocalVia) {
		t.Fatalf("approved_by = %v, want the approving principal", row["approved_by"])
	}

	id, _ := row["id"].(string)
	rec := serve(h, localPeer(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/grants/"+id+"/revoke", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	// The revoker is recorded and answered back.
	var rv map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rv); err != nil || rv["id"] != id || rv["revoked_by"] != "bridge:uid:501@"+string(auth.LocalVia) || rv["revoked_at"] == nil {
		t.Fatalf("revoke body %s: %v", rec.Body.String(), err)
	}
	if c := bearer(tok); c != http.StatusUnauthorized {
		t.Fatalf("token after revoke: %d, want 401", c)
	}
	if c := bearer(other); c != http.StatusOK {
		t.Fatalf("the other login after revoke: %d, want 200", c)
	}
	if list := listGrants(t, h); len(list.Grants) != 1 || list.Grants[0]["subject"] != "desktop" {
		t.Fatalf("a revoked login must leave the list: %+v", list.Grants)
	}
	rec = serve(h, localPeer(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/grants/nope/revoke", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d %s", rec.Code, rec.Body.String())
	}
}

// The web UI's own fetch, with the browser proof, lists and revokes.
func TestGrantsAPI_BrowserProofAdmits(t *testing.T) {
	f := newKBFixture(t)
	h := f.s.Handler()
	f.signIn(t, "laptop")
	rec := serve(h, browserReq(http.MethodGet, "/api/v1/oauth/grants", "", "127.0.0.1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list grantsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Grants) != 1 {
		t.Fatalf("list body %s: %v", rec.Body.String(), err)
	}
	id, _ := list.Grants[0]["id"].(string)
	rec = serve(h, browserReq(http.MethodPost, "/api/v1/oauth/grants/"+id+"/revoke", "{}", "127.0.0.1"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked_by":"anonymous@none"`) {
		t.Fatalf("revoke must record the browser principal: %d %s", rec.Code, rec.Body.String())
	}
}

// The same gate as the pending requests: anonymous loopback WITHOUT the
// browser proof, a bearer, and an operator certificate holding admin are each
// refused, and a refused revoke leaves the login working.
func TestGrantsAPI_OnlyLocalOrSameOriginAdmin(t *testing.T) {
	f := newKBFixture(t)
	h := f.s.Handler()
	tok := f.signIn(t, "laptop")
	fams, err := f.store.ListLogins(context.Background())
	if err != nil || len(fams) != 1 {
		t.Fatalf("logins = %+v, %v", fams, err)
	}
	id := fams[0].ID
	admin := f.mint(t, "admin-token", testIssuer, []string{"read", "write"}, auth.Read, auth.Write, auth.Admin)
	for name, req := range map[string]*http.Request{
		"anonymous loopback list":   fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/grants", nil)),
		"anonymous loopback revoke": fromLoopback(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/grants/"+id+"/revoke", strings.NewReader(`{}`))),
	} {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+admin)
		if rec := serve(h, req); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "local") {
			t.Errorf("%s: %d %s; want 403 naming the local listener", name, rec.Code, rec.Body.String())
		}
	}
	// A browser request missing one proof (here, the custom header).
	req := browserReq(http.MethodPost, "/api/v1/oauth/grants/"+id+"/revoke", "{}", "127.0.0.1")
	req.Header.Del(KnomitClientHeader)
	if rec := serve(h, req); rec.Code != http.StatusForbidden {
		t.Errorf("browser without %s: %d %s", KnomitClientHeader, rec.Code, rec.Body.String())
	}
	pk := pkitest.New(t)
	op := pk.Enroll(t, "op@example.com")
	opP := auth.OperatorPrincipal(op.Fingerprint())
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), opP, perm, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if rec := serve(asTLSPeer(h, op.Cert), httptest.NewRequest(http.MethodPost, "/api/v1/oauth/grants/"+id+"/revoke", nil)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), opP.String()) {
		t.Errorf("operator certificate with admin: %d %s; want 403 naming it", rec.Code, rec.Body.String())
	}
	if _, err := f.store.LookupAccess(context.Background(), tok); err != nil {
		t.Fatalf("a refused revoke ended the login: %v", err)
	}
}

// Without [oauth] the endpoints do not exist, which is what hides the
// Approved group in the UI.
func TestGrantsAPI_AbsentWithoutOAuth(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault}}
	rec := serve(s.Handler(), localPeer(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/grants", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("[oauth] unset: %d", rec.Code)
	}
}
