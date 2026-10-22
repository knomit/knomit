//go:build !windows

package auth

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The Windows counterpart is TestPeerCred_PipeReportsOwnSID.
func TestPeerCred_UnixSocketReportsOwnUID(t *testing.T) {
	// A short path: macOS caps sun_path at 104 bytes and t.TempDir() can exceed it.
	dir, err := os.MkdirTemp("/tmp", "pc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer server.Close()

	peer, ok := PeerCred(server)
	if !ok {
		t.Fatal("PeerCred must succeed on a unix connection")
	}
	if want := localID(os.Getuid()); peer.ID != want {
		t.Fatalf("id = %q, want %q", peer.ID, want)
	}
	if peer.Via != LocalVia {
		t.Fatalf("via = %q, want %q", peer.Via, LocalVia)
	}
	if peer.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d (linux SO_PEERCRED and darwin LOCAL_PEERPID both report it)", peer.PID, os.Getpid())
	}
}
