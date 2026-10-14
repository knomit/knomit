package web

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// tlsRequest builds a request that looks like it arrived on the TLS
// listener: the marker TLSConnContext sets, and r.TLS as net/http fills it.
// It proves only the MAPPING; TestTLSServer_* below proves the real path.
func tlsRequest(remote string, peers ...*x509.Certificate) *http.Request {
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = remote
	req.TLS = &tls.ConnectionState{PeerCertificates: peers}
	return req.WithContext(auth.TLSConnContext(req.Context(), nil))
}

func TestAuthMiddleware_TLSListenerMapsTheCertificateToItsPrincipal(t *testing.T) {
	f := pkitest.New(t)
	inst := f.Enroll(t, "peer@agents.knomit.io")
	op := f.Enroll(t, "alice@example.com")
	h := AuthMiddleware(config.AuthConfig{Require: false}, false)(principalEcho())
	for _, tc := range []struct {
		m    pkitest.Member
		want string
	}{
		{inst, "instance:" + inst.Fingerprint() + "@cert"},
		{op, "operator:" + op.Fingerprint() + "@cert"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, tlsRequest("10.0.0.7:5555", tc.m.Cert))
		if rr.Code != 200 || rr.Body.String() != tc.want {
			t.Fatalf("code=%d body=%q, want %q", rr.Code, rr.Body.String(), tc.want)
		}
		if len(tc.m.Fingerprint()) != 64 {
			t.Fatal("principal must carry the full fingerprint")
		}
	}
}

// The TLS listener is never anonymous: no certificate on a marked request is
// refused even with require=false and even from loopback, where plaintext
// would have made it anonymous.
func TestAuthMiddleware_TLSListenerWithoutCertIsRefusedEvenWithRequireFalse(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: false}, false)(principalEcho())
	for _, remote := range []string{"10.0.0.7:5555", "127.0.0.1:5555"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, tlsRequest(remote)) // r.TLS set, NO peer certificates
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "Authentication required") {
			t.Fatalf("%s: code=%d body=%s", remote, rr.Code, rr.Body.String())
		}
		// r.TLS nil on a marked request is refused the same way.
		req := httptest.NewRequest("GET", "/x", nil)
		req.RemoteAddr = remote
		req = req.WithContext(auth.TLSConnContext(req.Context(), nil))
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s with r.TLS nil: code=%d", remote, rr.Code)
		}
	}
	// Positive control: the SAME loopback request without the marker is the
	// anonymous principal, so the refusal above is the marker's doing.
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "localhost" // a browser on this machine; httptest's example.com is a rebound page (#281)
	req.TLS = &tls.ConnectionState{}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != "anonymous@none" {
		t.Fatalf("unmarked loopback: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// D18: the principal is the certificate's KEY, never its SAN. A leaf carrying
// a's SAN label and b's key (as if the verifier were bypassed) maps to b's
// fingerprint: nothing about a is read from the name. The key is the part of
// a certificate the TLS handshake proves possession of; the label is not.
func TestAuthMiddleware_TLSListenerPrincipalIsTheKeyNotTheSAN(t *testing.T) {
	f := pkitest.New(t)
	a := f.Enroll(t, "a@agents.knomit.io")
	b := f.Enroll(t, "b@agents.knomit.io")
	forged := *a.Cert
	forged.PublicKey = b.Cert.PublicKey // a's SAN, b's key
	h := AuthMiddleware(config.AuthConfig{}, false)(principalEcho())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, tlsRequest("10.0.0.7:5555", &forged))
	if want := "instance:" + b.Fingerprint() + "@cert"; rr.Code != 200 || rr.Body.String() != want {
		t.Fatalf("code=%d body=%q, want %q (the key's principal)", rr.Code, rr.Body.String(), want)
	}
	if strings.Contains(rr.Body.String(), a.Fingerprint()) {
		t.Fatal("a's identity was read from the SAN")
	}
}

// The regression test for the wrapper mistake: a REAL handshake through
// auth.ListenTLS and an http.Server with TLSConnContext, into AuthMiddleware.
// If ListenTLS ever wrapped the *tls.Conn, net/http would leave r.TLS nil
// and this would answer 403 instead of the principal.
func TestTLSServer_RealHandshakeReachesTheHandlerAsTheInstancePrincipal(t *testing.T) {
	f := pkitest.New(t)
	srvM := f.Enroll(t, "server@agents.knomit.io")
	dir := filepath.Join(t.TempDir(), "pki")
	f.Install(t, srvM, dir)
	cfg, err := pki.ServerConfig(dir, srvM.KeyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, closeLn, err := auth.ListenTLS("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLn()
	sawTLS := make(chan bool, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTLS <- r.TLS != nil
		principalEcho().ServeHTTP(w, r)
	})
	srv := &http.Server{
		Handler:     AuthMiddleware(config.AuthConfig{Require: false}, false)(inner),
		ConnContext: auth.TLSConnContext,
		ErrorLog:    log.New(io.Discard, "", 0),
	}
	go srv.Serve(ln)
	defer srv.Close()

	peer := f.Enroll(t, "peer@agents.knomit.io")
	resp, err := f.Client(t, peer).Get("https://" + ln.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if want := "instance:" + peer.Fingerprint() + "@cert"; resp.StatusCode != 200 || string(body) != want {
		t.Fatalf("code=%d body=%q, want %q", resp.StatusCode, body, want)
	}
	if !<-sawTLS {
		t.Fatal("the handler saw r.TLS == nil on a real TLS connection")
	}
}
