package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// A read-only anonymous loopback caller can GET but not POST.
func TestWriteGate_AnonymousLoopbackWithReadOnlyDefaultCannotMutate(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		Auth:    config.AuthConfig{Require: false, LoopbackDefault: []string{"read"}},
	}
	h := s.Handler()

	get := httptest.NewRequest("GET", "/api/v1/repos", nil)
	get.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != 200 {
		t.Fatalf("GET with read: %d %s", rr.Code, rr.Body.String())
	}

	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, post)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST without write must be 403, got %d %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("Permission denied")) {
		t.Fatalf("a principal that lacks write gets the PERMISSION refusal, not the authentication one: %s", rr.Body.String())
	}
}

// An EMPTY but present list is honoured as "anonymous holds nothing" — only
// an absent one falls back to the defaults.
func TestWriteGate_EmptyLoopbackDefaultGrantsNothing(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		Auth:    config.AuthConfig{LoopbackDefault: []string{}},
	}
	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("an explicitly empty loopback_default must deny: %d %s", rr.Code, rr.Body.String())
	}
}

// Defaults keep today's behaviour: anonymous loopback may mutate.
func TestWriteGate_DefaultLoopbackMayMutate(t *testing.T) {
	d := config.Defaults()
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: d.Auth}
	// The body is irrelevant: we assert only that the GATE did not answer.
	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, post)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("default loopback must not be gated: %s", rr.Body.String())
	}
}

// A Server literal with no Auth at all is what 73 existing test files build;
// it must behave as it did before [auth] existed.
func TestWriteGate_NilLoopbackDefaultFallsBackToDefaults(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, post)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("a Server built without config must not be gated: %s", rr.Body.String())
	}
}

func TestWriteGate_SocketPrincipalNeedsGrant(t *testing.T) {
	g := auth.StaticGrants{"bridge:uid:501@socket": {auth.Read: {}}}
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.AuthConfig{Require: true}, Grants: g}
	h := s.Handler()

	newPost := func() *http.Request {
		p := httptest.NewRequest("POST", "/api/v1/repos", nil)
		return p.WithContext(auth.WithPeer(p.Context(), 501, 7))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newPost())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("socket principal without write: %d %s", rr.Code, rr.Body.String())
	}
	g["bridge:uid:501@socket"] = auth.Set{auth.Read: {}, auth.Write: {}}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, newPost())
	if rr.Code == http.StatusForbidden {
		t.Fatalf("granted write still refused: %s", rr.Body.String())
	}
}

// The MCP routes are POST-for-reads. Gating them by HTTP method would make a
// read-only instance unreachable through the bridge: initialize and
// knomit_bind are themselves POSTs, so nothing could ever be bound and
// therefore nothing read. MCP write enforcement is per TOOL, in
// internal/mcp, not per HTTP method.
func TestWriteGate_MCPRoutesAreNotMethodGated(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		Auth:    config.AuthConfig{LoopbackDefault: []string{"read"}},
	}
	h := s.Handler()
	for _, path := range []string{
		"/api/v1/mcp",
		"/api/v1/repos/alpha/branches/agent:test/mcp",
		"/api/v1/lenses/somelens/mcp",
	} {
		req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(`{}`)))
		req.RemoteAddr = "127.0.0.1:1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusForbidden && bytes.Contains(rr.Body.Bytes(), []byte("Permission denied")) {
			t.Fatalf("%s must not be method-gated by writeGate: %s", path, rr.Body.String())
		}
	}
}

// /git is mounted on the OUTER router, so the gate has to wrap the mount.
// upload-pack is a FETCH — a read in git's terms — and must pass for a
// read-only principal; receive-pack (F11) is the push and must not.
func TestWriteGate_GitUploadPackExemptReceivePackGated(t *testing.T) {
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		GitHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }),
		Auth:       config.AuthConfig{LoopbackDefault: []string{"read"}},
	}
	h := s.Handler()

	for _, path := range []string{"/git/alpha/git-upload-pack", "/git/alpha.git/git-upload-pack"} {
		req := httptest.NewRequest("POST", path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 299 {
			t.Fatalf("%s is a fetch and must reach the handler: %d %s", path, rr.Code, rr.Body.String())
		}
	}

	req := httptest.NewRequest("POST", "/git/alpha/git-receive-pack", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("Permission denied")) {
		t.Fatalf("receive-pack must be refused by the GATE, not the handler: %d %s", rr.Code, rr.Body.String())
	}
}

// ReadOnly is a property of the INSTANCE, not of the caller, so its message
// has to win over a permission message even for a principal that holds write.
func TestReadOnlyGate_StillWinsOverWriteGate(t *testing.T) {
	s := &Server{
		Manager:  newTestManagerWithRepos(t, "alpha"),
		ReadOnly: true,
		Auth:     config.Defaults().Auth,
	}
	post := httptest.NewRequest("POST", "/api/v1/repos", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusForbidden || !bytes.Contains(rr.Body.Bytes(), []byte("Read-only instance")) {
		t.Fatalf("read-only message must win: %d %s", rr.Code, rr.Body.String())
	}
}
