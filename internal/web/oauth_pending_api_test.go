package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// localPeer is the kernel-verified caller on the local listener, spelled the
// way THIS platform's listener spells it (socket uid, or pipe SID).
func localPeer(r *http.Request) *http.Request {
	return r.WithContext(auth.WithPeer(r.Context(), auth.Peer{ID: "uid:501", Via: auth.LocalVia, PID: 7}))
}

// park drives a real /oauth/authorize on the OAuth router and returns the
// pending id.
func (f *oauthWebFixture) park(t *testing.T) string {
	t.Helper()
	q := url.Values{
		"response_type": {"code"}, "client_id": {"kb"}, "redirect_uri": {"http://127.0.0.1:5555/callback"},
		"code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"},
		"state": {"s"}, "scope": {"read"}, "resource": {testIssuer},
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "park-test")
	if rec := serve(f.s.OAuthHandler(), req); rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	list, err := f.s.OAuthIssuer.Pending(context.Background())
	if err != nil || len(list) == 0 {
		t.Fatalf("nothing pending: %v", err)
	}
	return list[len(list)-1].ID
}

func newKBFixture(t *testing.T) *oauthWebFixture {
	f := newOAuthWebFixture(t, testIssuer)
	// The kb client, as a configured instance has it.
	o := config.Defaults().OAuth
	f.s.OAuthIssuer = oauth.NewIssuer(oauth.Options{
		Issuer: testIssuer, Store: f.store, Clients: oauth.NewResolver(o.EffectiveClients()), Grants: f.grants,
	})
	// What app.seedOwnPrincipal gives the server's own account at boot.
	local := auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.LocalVia}
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), local, perm, "boot"); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// The local operator — and ONLY a local socket/pipe principal — lists,
// approves and denies. The list carries what the operator judges by.
func TestPendingAPI_LocalPrincipalListsAndApproves(t *testing.T) {
	f := newKBFixture(t)
	id := f.park(t)
	h := f.s.Handler()

	rec := serve(h, localPeer(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/pending", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Pending []map[string]any `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Pending) != 1 {
		t.Fatalf("list body %s: %v", rec.Body.String(), err)
	}
	row := list.Pending[0]
	for _, k := range []string{"id", "client_id", "client_name", "redirect_uri", "scopes", "remote_addr", "user_agent", "resource", "created_at", "expires_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("pending row lacks %q: %v", k, row)
		}
	}
	if row["user_agent"] != "park-test" || row["id"] != id {
		t.Fatalf("row = %v", row)
	}

	body := strings.NewReader(`{"subject":"laptop","scopes":["read","write"]}`)
	req := localPeer(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/approve", body))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(h, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"approved"`) {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := f.grants.List(context.Background(), oauth.TokenPrincipal("laptop").String())
	if len(rows) != 2 || rows[0].GrantedBy != "bridge:uid:501@"+string(auth.LocalVia) {
		t.Fatalf("grants must be written and name the approving principal: %+v", rows)
	}
}

func TestPendingAPI_Deny(t *testing.T) {
	f := newKBFixture(t)
	id := f.park(t)
	rec := serve(f.s.Handler(), localPeer(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/deny", nil)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := f.s.OAuthIssuer.Pending(context.Background()); len(list) != 0 {
		t.Fatalf("still pending after deny: %+v", list)
	}
}

// R5: anonymous loopback holds admin by default, and still may not approve;
// neither may a bearer (it is ignored on this listener anyway) or a
// certificate. This closes the CSRF path: a browser cannot reach the socket.
func TestPendingAPI_OnlyLocalPrincipals(t *testing.T) {
	f := newKBFixture(t)
	id := f.park(t)
	h := f.s.Handler()
	tok := f.mint(t, "admin-token", testIssuer, []string{"read", "write"}, auth.Read, auth.Write, auth.Admin)
	for name, req := range map[string]*http.Request{
		"anonymous loopback list":    fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/pending", nil)),
		"anonymous loopback approve": fromLoopback(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/approve", strings.NewReader(`{"subject":"x"}`))),
		"anonymous loopback deny":    fromLoopback(httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/deny", nil)),
	} {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := serve(h, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "local") {
			t.Errorf("%s: %d %s; want 403 naming the local listener", name, rec.Code, rec.Body.String())
		}
	}
	// An operator CERTIFICATE holding admin by row: a verified principal
	// with an id, and still not a local one.
	pk := pkitest.New(t)
	op := pk.Enroll(t, "op", pki.RoleOperator)
	opP := auth.OperatorPrincipal(op.Fingerprint())
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), opP, perm, "test"); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/pending/"+id+"/deny", nil)
	if rec := serve(asTLSPeer(h, op.Cert), req); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), opP.String()) {
		t.Errorf("operator certificate with admin: %d %s; want 403 naming it", rec.Code, rec.Body.String())
	}
	if list, _ := f.s.OAuthIssuer.Pending(context.Background()); len(list) != 1 {
		t.Fatal("a refused call decided the request")
	}
}

func TestPendingAPI_Errors(t *testing.T) {
	f := newKBFixture(t)
	id := f.park(t)
	h := f.s.Handler()
	post := func(path, body string) int {
		req := localPeer(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		return serve(h, req).Code
	}
	if c := post("/api/v1/oauth/pending/nope/approve", `{"subject":"x"}`); c != http.StatusNotFound {
		t.Errorf("unknown id: %d", c)
	}
	if c := post("/api/v1/oauth/pending/"+id+"/approve", `{"subject":"bad subject"}`); c != http.StatusBadRequest {
		t.Errorf("bad subject: %d", c)
	}
	if c := post("/api/v1/oauth/pending/"+id+"/approve", `{"subject":"x","scopes":["admin"]}`); c != http.StatusBadRequest {
		t.Errorf("admin scope: %d", c)
	}
	if c := post("/api/v1/oauth/pending/"+id+"/approve", `not json`); c != http.StatusBadRequest {
		t.Errorf("bad body: %d", c)
	}
	if c := post("/api/v1/oauth/pending/"+id+"/deny", ``); c != http.StatusNoContent {
		t.Fatalf("deny: %d", c)
	}
	if c := post("/api/v1/oauth/pending/"+id+"/approve", `{"subject":"x"}`); c != http.StatusConflict {
		t.Errorf("already decided: %d", c)
	}
}

// Without [oauth] the endpoints do not exist.
func TestPendingAPI_AbsentWithoutOAuth(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault}}
	rec := serve(s.Handler(), localPeer(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/pending", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("[oauth] unset: %d", rec.Code)
	}
}
