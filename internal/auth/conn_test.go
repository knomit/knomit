package auth

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConnContext_UnixCarriesPeer_TCPDoesNot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix sockets")
	}
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
	defer srv.Close()

	uid, pid, ok := PeerFromContext(ConnContext(context.Background(), srv))
	if !ok || uid != os.Getuid() {
		t.Fatalf("unix conn: uid=%d ok=%v, want uid %d", uid, ok, os.Getuid())
	}
	if pid != os.Getpid() {
		t.Fatalf("unix conn: pid=%d, want %d", pid, os.Getpid())
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
	defer ts.Close()
	if _, _, ok := PeerFromContext(ConnContext(context.Background(), ts)); ok {
		t.Fatal("tcp conn must not carry a peer")
	}
}

func TestPeerFromContext_AbsentIsFalse(t *testing.T) {
	if _, _, ok := PeerFromContext(context.Background()); ok {
		t.Fatal("a bare context carries no peer")
	}
	uid, pid, ok := PeerFromContext(WithPeer(context.Background(), 501, 4242))
	if !ok || uid != 501 || pid != 4242 {
		t.Fatalf("WithPeer round trip: uid=%d pid=%d ok=%v", uid, pid, ok)
	}
}
