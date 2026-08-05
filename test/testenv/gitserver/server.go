package gitserver

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Server is an in-process smart-HTTP git server backed by real git.
type Server struct {
	URL       string
	http      *httptest.Server
	plan      *FaultPlan
	closed    chan struct{}
	closeOnce sync.Once
}

// New starts a server exporting bare repos under projectRoot.
func New(t testing.TB, projectRoot string) *Server {
	t.Helper()
	plan := newFaultPlan()
	closed := make(chan struct{})
	cgiH := newCGIHandler(t, projectRoot)
	mw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := classify(r)
		if plan.hangFor(class) {
			// Block until the client gives up OR the server is closed.
			// Selecting on `closed` lets Close() drain a hung handler;
			// httptest.Server.Close() waits for outstanding requests, so
			// without this a stalled clone would deadlock teardown.
			select {
			case <-r.Context().Done():
			case <-closed:
			}
			return
		}
		if code := plan.statusFor(class); code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		user, pass, hasAuth := r.BasicAuth()
		if code := plan.checkAuth(user, pass, hasAuth); code != 0 {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", code)
			return
		}
		truncate, th := plan.bodyFaults(class)
		if truncate > 0 || th.perWrite > 0 {
			w = &faultWriter{ResponseWriter: w, truncateAfter: truncate, perWrite: th.perWrite, delay: th.delay}
		}
		cgiH.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(mw)
	return &Server{URL: ts.URL, http: ts, plan: plan, closed: closed}
}

// BasicAuth is the credential a server built by NewWithAuth demands on every
// request. It exists so a test can prove the product clones a GENUINELY private
// origin rather than a public one standing in for it: file:// and open-HTTP
// origins never authenticate, so a credential-recovery test against one passes
// even when the credential is dropped on the floor.
type BasicAuth struct{ User, Pass string }

// NewWithAuth is New plus HTTP basic auth on every request: a request whose
// credentials are missing or wrong gets 401 with a
// `WWW-Authenticate: Basic realm="git"` header.
//
// The check is the FaultPlan's — the same one ExpireAfter drives — rather than a
// second handler wrapped around it. That keeps one auth decision per request,
// leaves the fault-injection middleware outermost (so hangs and injected status
// codes still fire before any credential is looked at, exactly as for New), and
// means an auth-requiring server remains fully fault-injectable.
func NewWithAuth(t testing.TB, projectRoot string, auth BasicAuth) *Server {
	t.Helper()
	srv := New(t, projectRoot)
	srv.plan.RequireBasicAuth(auth.User, auth.Pass)
	return srv
}

// Close shuts the server down. It first drains any hung handlers (so a stalled
// clone cannot deadlock httptest.Server.Close, which waits for outstanding
// requests), then closes the underlying server.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
	s.http.Close()
}

// Fault returns the live FaultPlan; tests mutate it to inject failures.
func (s *Server) Fault() *FaultPlan { return s.plan }
