package serveraddr

import (
	"strings"
	"testing"
)

// Windows' "no event" answer depends on whether server.json's pid is alive
// (Local\ objects are per session). The selection is platform-free.
func TestMissingEventError_NamesTheOtherSessionWhenThePidIsAlive(t *testing.T) {
	alive := missingEventError(4242, true)
	if alive == nil || !strings.Contains(alive.Error(), "different Windows session") || !strings.Contains(alive.Error(), "4242") {
		t.Fatalf("live pid: %v", alive)
	}
	dead := missingEventError(4242, false)
	if dead == nil || strings.Contains(dead.Error(), "different Windows session") || !strings.Contains(dead.Error(), "no running knomit serve") {
		t.Fatalf("dead pid: %v", dead)
	}
}

func TestReloadListenerFailures_OnlyAFailedListenerCounts(t *testing.T) {
	ok := LockInfo{Listeners: map[string]LockListener{
		"http": {Reason: "off"}, "tls": {Configured: "a", Listening: "a"},
	}}
	if bad := ReloadListenerFailures(ok); len(bad) != 0 {
		t.Fatalf("off and healthy are not failures: %v", bad)
	}
	// A listener configured before this instance has a certificate waits for
	// the create or join that installs one (which starts it): not a failure.
	// SABOTAGE: drop the no_certificate exclusion -> red.
	waiting := LockInfo{Listeners: map[string]LockListener{"tls": {Configured: "0.0.0.0:19279", Reason: "no_certificate"}}}
	if bad := ReloadListenerFailures(waiting); len(bad) != 0 {
		t.Fatalf("a listener waiting for its certificate is not a failure: %v", bad)
	}
	for _, reason := range []string{"addr_in_use", "bind_failed", "tls_failed", "refused"} {
		bad := LockInfo{Listeners: map[string]LockListener{"http": {Configured: "x", Reason: reason}}}
		failed := ReloadListenerFailures(bad)
		if len(failed) != 1 || !strings.Contains(failed[0], reason) {
			t.Fatalf("reason %s: %v", reason, failed)
		}
		err := &ReloadListenerError{Info: bad, Failed: failed}
		if !strings.Contains(err.Error(), reason) {
			t.Fatalf("reason %s missing from %v", reason, err)
		}
	}
}
