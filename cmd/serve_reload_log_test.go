package cmd

import "testing"

// A reload whose fleet listener waits for this instance's certificate is
// logged at Info, not as "reload: left as it was" at ERR (PR #545 review);
// every other failed item, and no_certificate while a listener is still
// bound, stays an error.
// SABOTAGE: drop the tlsListening condition -> the bound row reads waiting
// -> red; return false always -> the waiting row -> red.
func TestReloadItemWaits_OnlyTheUnboundFleetListenerWithoutACertificate(t *testing.T) {
	for _, c := range []struct {
		item, why, listening string
		want                 bool
	}{
		{"tls", "no_certificate", "", true},
		{"tls", "no_certificate", "0.0.0.0:19279", false},
		{"tls", "addr_in_use", "", false},
		{"tls", "tls_failed", "", false},
		{"http", "no_certificate", "", false},
	} {
		if got := reloadItemWaits(c.item, c.why, c.listening); got != c.want {
			t.Errorf("reloadItemWaits(%q, %q, %q) = %v, want %v", c.item, c.why, c.listening, got, c.want)
		}
	}
}
