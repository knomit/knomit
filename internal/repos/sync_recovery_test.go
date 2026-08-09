package repos

import (
	"testing"

	"knomit/internal/store"
)

func ptrString(s string) *string { return &s }

func TestRemoteStatusIsError(t *testing.T) {
	if remoteStatusIsError(nil) {
		t.Fatal("a NULL status is 'never attempted', not a failure")
	}
	if remoteStatusIsError(ptrString("ok")) {
		t.Fatal("'ok' must not read as a failure")
	}
	if !remoteStatusIsError(ptrString("error")) {
		t.Fatal("'error' must read as a failure")
	}
}

func TestShouldBroadcastSyncOK(t *testing.T) {
	noop := store.SyncResult{
		Main:  store.MainReconcileResult{Mode: store.ModeNoop},
		Agent: store.AgentReconcileResult{Mode: store.ModeNoop},
	}
	mainMoved := store.SyncResult{
		Main:  store.MainReconcileResult{Mode: store.ModeFF},
		Agent: store.AgentReconcileResult{Mode: store.ModeNoop},
	}
	agentMoved := store.SyncResult{
		Main:  store.MainReconcileResult{Mode: store.ModeNoop},
		Agent: store.AgentReconcileResult{Mode: store.ModeMerge},
	}
	// A zero Mode is the "step did not run / nothing to report" shape, which
	// must be treated exactly like noop rather than as a change.
	empty := store.SyncResult{}

	cases := []struct {
		name       string
		result     store.SyncResult
		wasFailing bool
		want       bool
	}{
		{"steady state, nothing changed", noop, false, false},
		{"steady state, empty modes", empty, false, false},
		{"main moved", mainMoved, false, true},
		{"agent moved", agentMoved, false, true},
		// The reported bug: a transient outage recovers on a tick with nothing
		// to pull. Without this edge the client's banner is never lowered.
		{"recovery with nothing to pull", noop, true, true},
		{"recovery with empty modes", empty, true, true},
		{"recovery that also pulled", mainMoved, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBroadcastSyncOK(tc.result, tc.wasFailing); got != tc.want {
				t.Fatalf("shouldBroadcastSyncOK = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldBroadcastPushOK(t *testing.T) {
	if shouldBroadcastPushOK(false, false) {
		t.Fatal("a push with nothing to send and no prior failure is silent")
	}
	if !shouldBroadcastPushOK(true, false) {
		t.Fatal("an actual push must be broadcast")
	}
	// Same recovery edge as sync: an expired token that starts working again
	// usually has nothing left to push, so 'pushed' alone never clears the banner.
	if !shouldBroadcastPushOK(false, true) {
		t.Fatal("recovery from a push failure must be broadcast even with nothing to send")
	}
	if !shouldBroadcastPushOK(true, true) {
		t.Fatal("a push that also recovers must be broadcast")
	}
}
