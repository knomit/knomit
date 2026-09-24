package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/test/testenv"
)

// [oauth] off (the default): no OAuth router exists at all, so `knomit
// serve` opens no listener for it. On: the router is built over this
// instance's control.db and serves the issuer's documents and the 401.
func TestApp_OAuthWiredOnlyWhenConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if a.OAuthHandler() != nil {
		a.Close()
		t.Fatal("an OAuth router exists with [oauth] unset")
	}
	a.Close()

	cfg = config.Defaults()
	cfg.Home = t.TempDir()
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	a, err = New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot with [oauth]: %v", err)
	}
	defer a.Close()
	h := a.OAuthHandler()
	if h == nil {
		t.Fatal("no OAuth router with [oauth] set")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"issuer":"http://127.0.0.1:19280"`) {
		t.Fatalf("metadata: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
	req.RemoteAddr = "127.0.0.1:1"
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("API without a token on the OAuth router: %d", rr.Code)
	}
	// The plain handler is untouched by [oauth]: loopback anonymous as ever.
	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain handler changed by [oauth]: %d", rr.Code)
	}
}
