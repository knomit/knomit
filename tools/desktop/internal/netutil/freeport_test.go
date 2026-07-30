package netutil_test

import (
	"fmt"
	"net"
	"strconv"
	"testing"

	"knomit/tools/desktop/internal/netutil"
)

func TestListen_ReturnsBoundLooknomitckListener(t *testing.T) {
	ln, err := netutil.Listen("")
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
	ln2, err := netutil.Listen("")
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

	ln, err := netutil.Listen("")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if port := ln.Addr().(*net.TCPAddr).Port; port == netutil.PreferredPort {
		t.Errorf("Listen returned occupied preferred port %d", port)
	}
}

func TestListenUsesTheConfiguredPort(t *testing.T) {
	// Find a free port by taking one and releasing it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, err := netutil.Listen(strconv.Itoa(want))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if got := ln.Addr().(*net.TCPAddr).Port; got != want {
		t.Errorf("bound port = %d, want the configured %d", got, want)
	}
}

// An empty port is the ordinary case: knomit.toml need not mention it.
//
// The original version of this test asserted `got != PreferredPort && got ==
// 0`, which can never be true: net.Listen("tcp", "127.0.0.1:0") does not
// legitimately hand back port 0, and a real error there is already caught by
// the preceding t.Fatalf. That made the check unreachable — it passed even if
// Listen stopped trying PreferredPort altogether. Probing PreferredPort's
// availability first, and asserting the one branch that actually applies,
// keeps the test meaningful in CI (where 19278 is normally free) and on a
// busy dev machine (where it may not be) without ever skipping.
func TestListenFallsBackToPreferredWhenUnset(t *testing.T) {
	probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", netutil.PreferredPort))
	preferredFree := err == nil
	if preferredFree {
		probe.Close()
	}

	ln, err := netutil.Listen("")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	got := ln.Addr().(*net.TCPAddr).Port
	switch {
	case preferredFree && got != netutil.PreferredPort:
		t.Errorf("bound port = %d, want the preferred %d (it was free)", got, netutil.PreferredPort)
	case !preferredFree && (got == 0 || got == netutil.PreferredPort):
		t.Errorf("bound port = %d, want a non-zero ephemeral port distinct from the occupied %d", got, netutil.PreferredPort)
	}
}

// A taken port must not be fatal — the app still has to start, and the settings
// dialog surfaces the difference between configured and effective.
func TestListenFallsBackToEphemeralWhenTaken(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	taken := held.Addr().(*net.TCPAddr).Port

	ln, err := netutil.Listen(strconv.Itoa(taken))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if got := ln.Addr().(*net.TCPAddr).Port; got == taken {
		t.Errorf("bound the taken port %d", got)
	}
}
