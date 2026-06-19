package netutil_test

import (
	"net"
	"testing"

	"knomit/tools/desktop/internal/netutil"
)

func TestListen_ReturnsBoundLooknomitckListener(t *testing.T) {
	ln, err := netutil.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr is not *net.TCPAddr: %T", ln.Addr())
	}
	if addr.Port <= 0 || addr.Port > 65535 {
		t.Fatalf("port out of range: %d", addr.Port)
	}
	// The listener is already bound — no TOCTOU re-bind. A second Listen() must
	// not hand back the same port while we still hold this one.
	ln2, err := netutil.Listen()
	if err != nil {
		t.Fatalf("second Listen: %v", err)
	}
	defer ln2.Close()
	if ln2.Addr().(*net.TCPAddr).Port == addr.Port {
		t.Errorf("Listen handed out an in-use port %d twice", addr.Port)
	}
}

func TestListen_AvoidsOccupiedPreferred(t *testing.T) {
	// Occupy the preferred port (19278) so Listen must fall back.
	occupied, err := net.Listen("tcp", "127.0.0.1:19278")
	if err != nil {
		t.Skip("19278 already in use by something else; skipping")
	}
	defer occupied.Close()

	ln, err := netutil.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if port := ln.Addr().(*net.TCPAddr).Port; port == netutil.PreferredPort {
		t.Errorf("Listen returned occupied preferred port %d", port)
	}
}
