package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"knomit/internal/config"
	"knomit/test/testenv"
)

// TestApp_AuthConfigReachesMiddleware is what makes the nil-LoopbackDefault
// fallback in web.Server.grants() acceptable.
//
// That fallback exists because 73 test files build a web.Server literal with
// no [auth] config, and nil-means-deny would have meant touching all of them
// for no security gain: there is exactly ONE production Server literal, the
// one in New below. The cost of the fallback is that if that literal ever
// stopped carrying cfg.Auth, every instance would silently fall back to the
// full default permission set and no test would notice. This test is what
// notices.
//
// It boots a real App through Options.Embedder — the production wiring with
// a deterministic embedder in place of the ONNX one, so no model download and
// no native runtime (PR #284) — and asserts BOTH halves of [auth] arrived:
// Require and LoopbackDefault, each through the handler.
// The two need separate boots because they are mutually exclusive: with
// require = true there is no anonymous principal for a loopback default to
// apply to.
func TestApp_AuthConfigReachesMiddleware(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.Auth.Require = true
	// require = true with no local listener fails the boot
	// (checkLocalListener); config.Defaults leaves Socket empty.
	cfg.Socket = filepath.Join(cfg.Home, "knomit.sock")
	cfg.Auth.LoopbackDefault = []string{"read"}

	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer a.Close()

	// Half 1 — Require reached AuthMiddleware. A non-loopback caller with no
	// credential must be refused with 403 titled "Authentication required";
	// the TITLE is what distinguishes this from writeGate's "Permission
	// denied", so assert it rather than the status alone.
	req := httptest.NewRequest("POST", "/api/v1/repos", nil)
	req.RemoteAddr = "10.0.0.7:1"
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("Authentication required")) {
		t.Fatalf("[auth].require did not reach the middleware: %d %s", rr.Code, rr.Body.String())
	}

	// Half 2 — LoopbackDefault reached grants(). It needs its own boot,
	// because with require = true there is no anonymous principal to hold
	// the loopback set: the two halves are mutually exclusive by design.
	//
	// The anonymous set is resolved inside web.Server.grants(), which is
	// unexported, so this asserts it the only way a consumer can — through
	// the gate. loopback_default = [read] means a loopback MUTATION is
	// refused; if cfg.Auth stopped reaching the literal, LoopbackDefault
	// would be nil, grants() would fall back to the full default set, and
	// this POST would sail through.
	cfg2 := config.Defaults()
	cfg2.Home = t.TempDir()
	cfg2.Auth.Require = false
	cfg2.Auth.LoopbackDefault = []string{"read"}

	a2, err := New(context.Background(), cfg2, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	defer a2.Close()

	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	post.Host = "localhost" // a browser on this machine; httptest's example.com is a rebound page (#281)
	rr = httptest.NewRecorder()
	a2.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("Permission denied")) {
		t.Fatalf("[auth].loopback_default did not reach grants(): %d %s", rr.Code, rr.Body.String())
	}

	// And a READ on the same instance still works, so the refusal above is
	// the permission gate and not a broken boot.
	get := httptest.NewRequest("GET", "/api/v1/repos", nil)
	get.RemoteAddr = "127.0.0.1:1"
	get.Host = "localhost" // a browser on this machine; httptest's example.com is a rebound page (#281)
	rr = httptest.NewRecorder()
	a2.Handler().ServeHTTP(rr, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("read must still work with loopback_default = [read]: %d %s", rr.Code, rr.Body.String())
	}
}
