package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/client/sessions"
	"knomit/internal/config"
)

// principalEcho writes the principal the middleware attached, or 204 when it
// attached none — so a test can tell "anonymous" from "nothing" apart.
func principalEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(p.String()))
	})
}

func TestAuthMiddleware_LoopbackWithoutRequireIsAnonymous(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: false}, false)(principalEcho())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "localhost" // a browser on this machine; httptest's example.com is a rebound page (#281)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != "anonymous@none" {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestAuthMiddleware_RequireRefusesUnauthenticated(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: true}, false)(principalEcho())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "localhost" // a browser on this machine; httptest's example.com is a rebound page (#281)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("Authentication required")) {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("phase 1 has no scheme to advertise; a challenge header here would be a lie until phase 3")
	}
}

func TestAuthMiddleware_NonLoopbackWithoutRequireHasNoPrincipal(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: false}, false)(principalEcho())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "10.0.0.7:5555"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("a remote caller must not become anonymous-loopback: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// testPeer is a peer credential as auth.ConnContext would have attached one.
//
// The ID and Via are LITERAL, and stay literal on every platform: since
// knomit#245 the middleware COPIES them out of the Peer rather than
// formatting a uid itself, so what these tests pin is the copying. Which
// spelling a platform actually produces (uid: or sid:) is
// auth.localID and auth.LocalVia, tested against the real OS in
// internal/auth.
func testPeer(uid, pid int) auth.Peer {
	return auth.Peer{ID: "uid:" + strconv.Itoa(uid), Via: auth.ViaSocket, PID: pid}
}

func TestAuthMiddleware_SocketPeerBecomesBridgePrincipal(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: true}, false)(principalEcho())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "@" // a unix conn has no ip; the peer in ctx is what counts
	req = req.WithContext(auth.WithPeer(req.Context(), testPeer(501, 4242)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Body.String() != "bridge:uid:501@socket" {
		t.Fatalf("body=%q", rr.Body.String())
	}
}

// The kernel's pid has to reach the session recorder, or client_sessions
// keeps only the pid the client declared about itself.
func TestAuthMiddleware_SocketPeerCarriesVerifiedPID(t *testing.T) {
	var gotPID int
	var gotOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPID, gotOK = sessions.VerifiedPIDFromContext(r.Context())
	})
	h := AuthMiddleware(config.AuthConfig{Require: true}, false)(inner)
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(auth.WithPeer(req.Context(), testPeer(501, 4242)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !gotOK || gotPID != 4242 {
		t.Fatalf("verified pid = %d ok=%v, want 4242", gotPID, gotOK)
	}
}

// isLoopback must stay narrow: widening it is the tempting way to fix a
// collateral test failure and it would hand every LAN caller the anonymous
// principal's rights.
func TestIsLoopback_OnlyLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:1", "127.9.9.9:1", "[::1]:1"} {
		if !isLoopback(addr) {
			t.Fatalf("%q must be loopback", addr)
		}
	}
	for _, addr := range []string{"10.0.0.7:1", "192.0.2.1:1234", "[fe80::1]:1", "", "garbage"} {
		if isLoopback(addr) {
			t.Fatalf("%q must NOT be loopback", addr)
		}
	}
}

func TestRequire_DeniesMissingPermissionWith403NamingIt(t *testing.T) {
	g := auth.StaticGrants{"bridge:uid:501@socket": {auth.Read: {}}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := Require(g, auth.Write)(inner)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindBridge, ID: "uid:501", Via: auth.ViaSocket})
	req := httptest.NewRequest("POST", "/x", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("write")) {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("Permission denied")) {
		t.Fatalf("the title must distinguish this from the no-principal refusal: %s", rr.Body.String())
	}
	h = Require(g, auth.Read)(inner)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("granted read refused: %d", rr.Code)
	}
}

func TestLoopbackGrants_AnonymousComesFromConfigNotTheStore(t *testing.T) {
	g := loopbackGrants{
		anon:  auth.Set{auth.Read: {}},
		store: auth.StaticGrants{"anonymous@none": {auth.Write: {}}},
	}
	set, err := g.For(context.Background(), auth.Principal{Kind: auth.KindAnonymous, Via: auth.ViaNone})
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has(auth.Read) || set.Has(auth.Write) {
		t.Fatalf("anonymous must come from config, not from a grants row: %v", set)
	}
}

// Form B must attach a principal, not merely skip the gate: a handler that
// reads auth.FromContext has to see anonymous, the same as on loopback.
func TestAuthMiddleware_DisabledAttachesAnonymousRegardlessOfAddress(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: true}, true)(principalEcho())
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Body.String() != "anonymous@none" {
		t.Fatalf("body=%q", rr.Body.String())
	}
}
