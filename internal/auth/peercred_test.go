package auth

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPeerCred_UnixSocketReportsOwnUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix sockets")
	}
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
	defer server.Close()

	uid, pid, ok := PeerCred(server)
	if !ok {
		t.Fatal("PeerCred must succeed on a unix connection")
	}
	if uid != os.Getuid() {
		t.Fatalf("uid = %d, want %d", uid, os.Getuid())
	}
	if pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d (linux SO_PEERCRED and darwin LOCAL_PEERPID both report it)", pid, os.Getpid())
	}
}

func TestPeerCred_TCPIsNotOK(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	if _, _, ok := PeerCred(server); ok {
		t.Fatal("a TCP connection has no peer credentials")
	}
}
