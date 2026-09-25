package hostguard

import "testing"

func TestLoopbackHostOK(t *testing.T) {
	listed := []string{"box.tail1234.ts.net"}
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"", true},
		{"localhost", true},
		{"LocalHost:6060", true},
		{"127.0.0.1:6060", true},
		{"[::1]", true},
		{"[::1]:6060", true},
		{"10.1.2.3:6060", true}, // any IP literal: rebinding needs a name
		{"box.tail1234.ts.net:443", true},
		{"BOX.TAIL1234.TS.NET", true},
		{"attacker.example:6060", false},
		{"localhost.attacker.example", false},
		{"app.localhost", false},
		{"localhost.", false},
		{"127.0.0.1.nip.io", false},
		{"tail1234.ts.net", false},
	} {
		if got := LoopbackHostOK(c.host, listed); got != c.ok {
			t.Errorf("LoopbackHostOK(%q) = %v, want %v", c.host, got, c.ok)
		}
	}
	if LoopbackHostOK("box.tail1234.ts.net", nil) {
		t.Error("a name admitted with no list: the listed names must come from the caller")
	}
}

func TestLoopbackPeer(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:1", "127.9.9.9:1", "[::1]:1", "::1", "127.0.0.1"} {
		if !LoopbackPeer(addr) {
			t.Errorf("LoopbackPeer(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"10.0.0.7:1", "192.0.2.1:1234", "[fe80::1]:1", "0.0.0.0:1", "[::]:1", "", "garbage", "@"} {
		if LoopbackPeer(addr) {
			t.Errorf("LoopbackPeer(%q) = true, want false", addr)
		}
	}
}
