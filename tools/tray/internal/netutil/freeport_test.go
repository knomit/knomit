package netutil_test

import (
	"fmt"
	"net"
	"testing"

	"knomit/tools/tray/internal/netutil"
)

func TestPickPort_ReturnsFreeLooknomitckPort(t *testing.T) {
	port, err := netutil.PickPort()
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port out of range: %d", port)
	}
	// Confirm we can bind to it (it's actually free right now).
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("bind picked port %d: %v", port, err)
	}
	ln.Close()
}

func TestPickPort_AvoidsOccupiedPreferred(t *testing.T) {
	// Occupy the preferred port (19278) so PickPort must fall back.
	ln, err := net.Listen("tcp", "127.0.0.1:19278")
	if err != nil {
		t.Skip("19278 already in use by something else; skipping")
	}
	defer ln.Close()

	port, err := netutil.PickPort()
	if err != nil {
		t.Fatalf("PickPort: %v", err)
	}
	if port == 19278 {
		t.Errorf("PickPort returned occupied preferred port")
	}
}
