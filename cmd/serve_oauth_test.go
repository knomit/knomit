package cmd

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"knomit/internal/config"
)

// Off unless [oauth] is configured: no listener, no error.
func TestOpenOAuthServer_OffByDefault(t *testing.T) {
	srv, ln, err := openOAuthServer(config.Defaults().OAuth, http.NotFoundHandler(), &http.Server{})
	if srv != nil || ln != nil || err != nil {
		t.Fatalf("[oauth] unset: %v %v %v", srv, ln, err)
	}
}

// On: its OWN http.Server on [oauth].addr serving the OAuth router — never
// the plain listener's handler — with the plain server's timeouts and base
// context.
func TestOpenOAuthServer_ServesItsOwnHandler(t *testing.T) {
	o := config.Defaults().OAuth
	o.Issuer, o.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	type ctxKey struct{}
	like := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "plain") }),
		ReadHeaderTimeout: 7 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithValue(context.Background(), ctxKey{}, "base") },
	}
	oauthH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "oauth:"+r.Context().Value(ctxKey{}).(string))
	})
	srv, ln, err := openOAuthServer(o, oauthH, like)
	if err != nil || srv == nil || ln == nil {
		t.Fatalf("open: %v %v %v", srv, ln, err)
	}
	if srv.ReadHeaderTimeout != 7*time.Second {
		t.Fatalf("timeouts not copied: %v", srv.ReadHeaderTimeout)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	resp, err := http.Get("http://" + ln.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "oauth:base" {
		t.Fatalf("served %q; want the OAuth handler with the plain server's base context", body)
	}
}

func TestOpenOAuthServer_BindFailureIsAnError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	o := config.Defaults().OAuth
	o.Issuer, o.Addr = "http://127.0.0.1:19280", busy.Addr().String()
	if _, _, err := openOAuthServer(o, http.NotFoundHandler(), &http.Server{}); err == nil {
		t.Fatal("binding a taken address succeeded")
	}
}
