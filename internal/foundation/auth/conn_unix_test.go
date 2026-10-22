//go:build !windows

package auth

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The Windows counterpart is TestConnContext_PipeCarriesPeer_TCPDoesNot in
// conn_windows_test.go. Both halves assert the same pair of facts about the
// platform's local transport; neither skips.
func TestConnContext_UnixCarriesPeer_TCPDoesNot(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	acc := make(chan net.Conn, 2)
	go func() { c, _ := ul.Accept(); acc <- c }()
	cl, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	srv := <-acc
	if srv == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer srv.Close()

	peer, ok := PeerFromContext(ConnContext(context.Background(), srv))
	if !ok {
		t.Fatal("a unix conn must carry a peer")
	}
	if want := localID(os.Getuid()); peer.ID != want {
		t.Fatalf("unix conn: id=%q, want %q", peer.ID, want)
	}
	if peer.Via != LocalVia {
		t.Fatalf("unix conn: via=%q, want %q", peer.Via, LocalVia)
	}
	if peer.PID != os.Getpid() {
		t.Fatalf("unix conn: pid=%d, want %d", peer.PID, os.Getpid())
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
