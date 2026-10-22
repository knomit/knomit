package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s: Content-Type %q", path, ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rec.Code, doc
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The AS metadata (RFC 8414): issuer-exact, PKCE S256 only, CIMD and the
// RFC 9207 iss parameter advertised, and no registration endpoint (DCR is
// 3b). Each advertisement is one a client fails closed or open on, so each
// is asserted by name.
func TestMetadata_AuthorizationServer(t *testing.T) {
	h := WellKnown("https://knomit.example.com")
	code, doc := getJSON(t, h, "/.well-known/oauth-authorization-server")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want := map[string]string{
		"issuer":                 "https://knomit.example.com",
		"authorization_endpoint": "https://knomit.example.com/oauth/authorize",
		"token_endpoint":         "https://knomit.example.com/oauth/token",
		"revocation_endpoint":    "https://knomit.example.com/oauth/revoke",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %q", k, doc[k], v)
		}
	}
	if got := strs(doc["code_challenge_methods_supported"]); !slices.Equal(got, []string{"S256"}) {
		t.Errorf("code_challenge_methods_supported = %v, want exactly [S256]", got)
	}
	if doc["client_id_metadata_document_supported"] != true {
		t.Error("CIMD not advertised")
	}
	if doc["authorization_response_iss_parameter_supported"] != true {
		t.Error("RFC 9207 iss not advertised: MCP's mix-up defence fails open without it")
	}
	if _, ok := doc["registration_endpoint"]; ok {
		t.Error("registration_endpoint advertised, but DCR is not built (3b)")
	}
	if got := strs(doc["response_types_supported"]); !slices.Equal(got, []string{"code"}) {
		t.Errorf("response_types_supported = %v", got)
	}
	if got := strs(doc["grant_types_supported"]); !slices.Equal(got, []string{"authorization_code", "refresh_token"}) {
		t.Errorf("grant_types_supported = %v", got)
	}
	if got := strs(doc["token_endpoint_auth_methods_supported"]); !slices.Equal(got, []string{"none"}) {
		t.Errorf("token_endpoint_auth_methods_supported = %v", got)
	}
	scopes := strs(doc["scopes_supported"])
	if !slices.Contains(scopes, "read") || !slices.Contains(scopes, "write") || slices.Contains(scopes, "admin") {
		t.Errorf("scopes_supported = %v: want read and write, never admin", scopes)
	}
}

// The protected-resource metadata (RFC 9728), root and path-suffix forms.
// §3.3: the `resource` in the document MUST equal the URL the client derived
// the metadata URL from, or the client rejects it — so the suffix form echoes
// its resource rather than returning the root document.
func TestMetadata_ProtectedResource(t *testing.T) {
	h := WellKnown("https://knomit.example.com")
	for path, resource := range map[string]string{
		"/.well-known/oauth-protected-resource":                    "https://knomit.example.com",
		"/.well-known/oauth-protected-resource/api/v1/repos/r/mcp": "https://knomit.example.com/api/v1/repos/r/mcp",
	} {
		code, doc := getJSON(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", path, code)
		}
		if doc["resource"] != resource {
			t.Errorf("%s: resource = %v, want %q", path, doc["resource"], resource)
		}
		if got := strs(doc["authorization_servers"]); !slices.Equal(got, []string{"https://knomit.example.com"}) {
			t.Errorf("%s: authorization_servers = %v", path, got)
		}
		if got := strs(doc["bearer_methods_supported"]); !slices.Equal(got, []string{"header"}) {
			t.Errorf("%s: bearer_methods_supported = %v", path, got)
		}
	}
}

// An issuer WITH a path: RFC 8414 §3 and RFC 9728 §3.1 insert the well-known
// segment between host and path, so neither document lives under /knomit, and
// the origin-root documents are not ours to answer. The proxy strips /knomit
// from everything else but forwards these unstripped (Deployment rule).
func TestMetadata_IssuerWithPath(t *testing.T) {
	h := WellKnown("https://host.example/knomit")
	code, doc := getJSON(t, h, "/.well-known/oauth-authorization-server/knomit")
	if code != http.StatusOK || doc["issuer"] != "https://host.example/knomit" ||
		doc["token_endpoint"] != "https://host.example/knomit/oauth/token" {
		t.Fatalf("AS metadata with path: %d %v", code, doc)
	}
	code, doc = getJSON(t, h, "/.well-known/oauth-protected-resource/knomit/api/v1/repos/r/mcp")
	if code != http.StatusOK || doc["resource"] != "https://host.example/knomit/api/v1/repos/r/mcp" {
		t.Fatalf("PRM with path: %d %v", code, doc)
	}
	code, doc = getJSON(t, h, "/.well-known/oauth-protected-resource/knomit")
	if code != http.StatusOK || doc["resource"] != "https://host.example/knomit" {
		t.Fatalf("PRM for the issuer itself: %d %v", code, doc)
	}
	for _, p := range []string{
		"/.well-known/oauth-authorization-server",            // the origin's, not ours
		"/.well-known/oauth-protected-resource",              // likewise
		"/.well-known/oauth-protected-resource/other/api",    // outside the issuer path
		"/.well-known/oauth-protected-resource/knomitx/api",  // A-vs-AB boundary
		"/.well-known/oauth-authorization-server/knomit/x",   // AS metadata has no suffix form
		"/.well-known/oauth-protected-resource/knomit/../x",  // escapes after cleaning
		"/.well-known/oauth-protected-resource/knomit/a//b",  // non-canonical
		"/.well-known/oauth-protected-resource/knomit/a%2Fb", // an encoded slash is not a segment break
	} {
		if code, _ := getJSON(t, h, p); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, code)
		}
	}
}

func TestMetadata_OnlyGET(t *testing.T) {
	h := WellKnown("https://knomit.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
