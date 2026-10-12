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
	for _, reason := range []string{"addr_in_use", "bind_failed", "tls_failed", "no_certificate", "refused"} {
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
