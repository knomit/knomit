//go:build windows

package auth

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// The Windows counterpart of TestConnContext_UnixCarriesPeer_TCPDoesNot.
// Same two facts: the platform's local transport carries a verified peer, and
// a TCP connection on the same server carries none.
func TestConnContext_PipeCarriesPeer_TCPDoesNot(t *testing.T) {
	path := testLocalListenerPath(t)
	pl, cleanup, err := ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	t.Cleanup(cleanup)
	acc := make(chan net.Conn, 2)
	go func() { c, _ := pl.Accept(); acc <- c }()
	cl, err := DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("DialLocal(%q): %v", path, err)
	}
	defer cl.Close()
	srv := <-acc
	if srv == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer srv.Close()

	peer, ok := PeerFromContext(ConnContext(context.Background(), srv))
	if !ok {
		t.Fatal("a pipe conn must carry a peer")
	}
	want, err := ownSID()
	if err != nil {
		t.Fatal(err)
	}
	if peer.ID != localID(want) {
		t.Fatalf("pipe conn: id=%q, want %q", peer.ID, localID(want))
	}
	if peer.Via != ViaPipe {
		t.Fatalf("pipe conn: via=%q, want %q", peer.Via, ViaPipe)
	}
	if peer.PID != os.Getpid() {
		t.Fatalf("pipe conn: pid=%d, want %d", peer.PID, os.Getpid())
	}

	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	go func() { c, _ := tl.Accept(); acc <- c }()
	tc, err := net.Dial("tcp", tl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	ts := <-acc
	if ts == nil {
		t.Fatal("accept produced no TCP connection; this fixture would prove nothing")
	}
	defer ts.Close()
	if _, ok := PeerFromContext(ConnContext(context.Background(), ts)); ok {
		t.Fatal("tcp conn must not carry a peer")
	}
}
