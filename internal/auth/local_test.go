package auth

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// THE invariant defect B (knomit#245) exists for: the principal the BOOT
// SEEDING writes (LocalPrincipal) and the principal the MIDDLEWARE derives
// from a real connection (PeerCred, then Peer.Principal) have to render the
// same string on this platform. If they ever differ, boot seeds a grants row
// that no request can match, and every local write is refused by a server
// that looks perfectly healthy.
//
// It asserts that by actually connecting this process to itself over the
// platform's real local listener, rather than by comparing two calls to the
// same function — which would pass however wrong both were.
func TestLocalPrincipal_AgreesWithWhatPeerCredSeesOfUs(t *testing.T) {
	path := testLocalListenerPath(t)
	l, err := ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()

	client, err := DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("DialLocal(%q): %v", path, err)
	}
	defer client.Close()

	server := <-accepted
	if server == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer server.Close()

	peer, ok := PeerCred(server)
	if !ok {
		t.Fatal("PeerCred found no credentials on our own local connection")
	}
	me, err := LocalPrincipal()
	if err != nil {
		t.Fatalf("LocalPrincipal: %v", err)
	}

	// POSITIVE CONTROL, first. The equality below is also satisfied by two
	// empty strings, and an ID that names nobody is exactly the failure this
	// test is here to catch ("uid:-1" on Windows was one).
	if me.ID == "" || me.Via == "" || me.Kind == "" {
		t.Fatalf("LocalPrincipal produced an empty field: %+v", me)
	}
	if me.Via != LocalVia {
		t.Fatalf("LocalPrincipal via = %q, want the platform's LocalVia %q", me.Via, LocalVia)
	}
	if !strings.HasPrefix(me.String(), "bridge:") || !strings.HasSuffix(me.String(), "@"+string(LocalVia)) {
		t.Fatalf("principal %q is not shaped bridge:<id>@%s", me.String(), LocalVia)
	}

	if peer.Principal().String() != me.String() {
		t.Fatalf("the middleware would name us %q but boot seeds %q — a seeded grant no request can match",
			peer.Principal().String(), me.String())
	}

	// The pid the OS reports has to be OURS: it is what lands in
	// client_session_peers.verified_pid, and the whole point of that column
	// is that it is not the pid the client declared about itself.
	if peer.PID != os.Getpid() {
		t.Fatalf("peer pid = %d, want this process's %d", peer.PID, os.Getpid())
	}
}

// The listener has to actually carry HTTP, not merely accept: a transport
// that connects and then cannot serve a request would pass the test above
// while giving the bridge nothing.
func TestListenLocal_ServesHTTPOverTheLocalTransport(t *testing.T) {
	path := testLocalListenerPath(t)
	l, err := ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	defer l.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := FromContext(r.Context())
			_, _ = io.WriteString(w, p.String())
		}),
		// The same hook cmd/serve installs: this is the only moment the conn
		// exists, so the principal has to be derived here.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			peer, ok := PeerCred(c)
			if !ok {
				return ctx
			}
			return WithPrincipal(ctx, peer.Principal())
		},
	}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return DialLocal(ctx, path, 10*time.Second)
		},
	}}
	// The host is ignored: the dialler above goes to the local transport
	// whatever the URL says. Port 1 answers nothing, so only the listener can.
	resp, err := c.Get("http://localhost:1/whoami")
	if err != nil {
		t.Fatalf("GET over the local listener: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	me, err := LocalPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != me.String() {
		t.Fatalf("the server saw %q, want %q", body, me.String())
	}
}
