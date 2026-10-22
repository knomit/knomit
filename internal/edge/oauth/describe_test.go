package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// R4 (F19 phase 3b): GET /oauth/pending/{id} on the OAuth listener tells
// whoever holds the id what that request asks for — the REQUESTER-SUPPLIED
// fields only — plus a digest over them. The operator signs the digest
// (signed approval), so a relay that shows the operator one request and
// hands the instance another produces a statement the instance refuses.

func TestDescribe_RequesterFieldsAndDigest(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	resp := f.get(t, "/oauth/pending/"+id, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("describe: %d Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	row, err := f.store.GetPending(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": id, "client_id": "kb", "client_name": row.ClientName,
		"redirect_uri": "http://127.0.0.1:54321/callback", "resource": f.srv.URL,
		"digest": RequestDigest(row),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if s, _ := json.Marshal(got["scopes"]); string(s) != `["read","write"]` {
		t.Errorf("scopes = %s", s)
	}
	// The requester's network details describe the browser; they are the
	// operator's business, not the requester's.
	for _, k := range []string{"remote_addr", "user_agent", "state", "code_challenge"} {
		if _, ok := got[k]; ok {
			t.Errorf("the public description carries %q", k)
		}
	}
}

// Every field the operator judges by moves the digest; the browser's
// details do not.
func TestRequestDigest_BindsEachRequesterField(t *testing.T) {
	base := Pending{ClientID: "https://c.example/m", ClientName: "C", RedirectURI: "http://localhost/callback",
		Resource: "https://k.example/api/v1/repos/kb/mcp", Scopes: []string{"read", "write"}, RemoteAddr: "1.2.3.4:5", UserAgent: "ua"}
	d := RequestDigest(base)
	if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not 64 lowercase hex", d)
	}
	for name, mut := range map[string]func(p *Pending){
		"client_id":    func(p *Pending) { p.ClientID = "https://evil.example/m" },
		"client_name":  func(p *Pending) { p.ClientName = "C2" },
		"redirect_uri": func(p *Pending) { p.RedirectURI = "https://evil.example/cb" },
		"resource":     func(p *Pending) { p.Resource = "https://k.example/api/v1/mcp" },
		"scopes":       func(p *Pending) { p.Scopes = []string{"read", "write", "operator"} },
		// A field boundary cannot be moved to make two requests collide.
		"boundary": func(p *Pending) { p.ClientID, p.ClientName = "https://c.example/mC", "" },
	} {
		p := base
		mut(&p)
		if RequestDigest(p) == d {
			t.Errorf("%s changed and the digest did not", name)
		}
	}
	p := base
	p.RemoteAddr, p.UserAgent = "9.9.9.9:9", "other"
	if RequestDigest(p) != d {
		t.Error("the browser's address or user agent moved the digest")
	}
}

// Only a waiting request is described: an unknown, malformed, decided or
// expired id is a 404, and nothing is retained for it (3a review B1).
func TestDescribe_OnlyWaitingRequests(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	for _, id := range []string{"nope", strings.Repeat("A", 43), strings.Repeat("A", 4096)} {
		if resp := f.get(t, "/oauth/pending/"+id, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("id %.20q: %d, want 404", id, resp.StatusCode)
		}
	}
	id := f.authorize(t, f.authorizeQuery(ch))
	if err := f.iss.Deny(context.Background(), id, "test"); err != nil {
		t.Fatal(err)
	}
	if resp := f.get(t, "/oauth/pending/"+id, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("decided: %d, want 404", resp.StatusCode)
	}
	id = f.authorize(t, f.authorizeQuery(ch))
	f.clock.add(pendingTTL + 1)
	if resp := f.get(t, "/oauth/pending/"+id, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("expired: %d, want 404", resp.StatusCode)
	}
	if n := f.waiterEntries(); n != 0 {
		t.Errorf("describe registered %d waiters", n)
	}
}
