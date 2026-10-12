package cmd

import (
	"strings"
	"testing"

	"knomit/internal/serveraddr"
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

func TestReloadOutcome_OnlyAFailedListenerIsAnError(t *testing.T) {
	ok := serveraddr.LockInfo{Listeners: map[string]serveraddr.LockListener{
		"http": {Reason: "off"}, "tls": {Configured: "a", Listening: "a"},
	}}
	if err := reloadOutcome(ok); err != nil {
		t.Fatalf("off and healthy are not failures: %v", err)
	}
	for _, reason := range []string{"addr_in_use", "bind_failed", "tls_failed", "no_certificate", "refused"} {
		bad := serveraddr.LockInfo{Listeners: map[string]serveraddr.LockListener{"http": {Configured: "x", Reason: reason}}}
		if err := reloadOutcome(bad); err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("reason %s: %v", reason, err)
		}
	}
}
