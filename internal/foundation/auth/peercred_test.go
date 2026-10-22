package auth

import (
	"net"
	"testing"
)

// A TCP connection must never yield a peer, on any platform: it is what stops
// a caller who merely reached the loopback port from being taken for the
// local user. Both per-platform PeerCred implementations open with the type
// check that enforces it, so this runs untagged.
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
	if server == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer server.Close()
	if _, ok := PeerCred(server); ok {
		t.Fatal("a TCP connection has no peer credentials")
	}
}
