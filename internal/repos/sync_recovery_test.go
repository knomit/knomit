package repos

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

// A tick that died because the LOOP was cancelled is not a failure of the
// remote, and must not be reported as one.
//
// The reported case: creating a repo against a remote ends with ActivateSync,
// which cancels this loop to restart it (builder.go). A tick whose fetch was
// in flight came back with "context canceled", and the loop broadcast that to
// the client and counted it toward failure escalation — so a create that
// fully succeeded put a red "sync failed" line on the repo screen.
//
// The test is the loop's own context, NOT the error's text: a fetch that hit
// its netTimeout against an unresponsive remote also carries a context error,
// but the loop is still live and that is a real failure worth reporting.
func TestTickAbandoned(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	// Wrapped with %w, exactly as store.Sync builds it: `fmt.Errorf("Sync:
	// fetch: %w", fetchErr)` over go-git's *url.Error over context.Canceled.
	// The predicate reads the CHAIN, never the text — an error that merely
	// says "context canceled" without carrying it is not a cancellation.
	err := fmt.Errorf("Sync: fetch: %w", context.Canceled)

	if tickAbandoned(live, err) {
		t.Fatal("a failure on a live loop is a real failure and must be reported")
	}
	if tickAbandoned(dead, nil) {
		t.Fatal("a tick that SUCCEEDED before the cancellation still succeeded")
	}
	if !tickAbandoned(dead, err) {
		t.Fatal("a failure caused by our own cancellation must not be reported")
	}

	// A DEADLINE is not a cancellation. A tick that ran out of time failed
	// against a remote that did not answer, and the user needs to know.
	timedOut, cancelTimeout := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelTimeout()
	<-timedOut.Done()
	if tickAbandoned(timedOut, fmt.Errorf("Sync: fetch: %w", context.DeadlineExceeded)) {
		t.Fatal("a tick that ran out of time is a real failure and must be reported")
	}

	// And a REAL failure that lands as the loop is torn down is still a real
	// failure: the cause is the remote's answer, not our cancellation.
	if tickAbandoned(dead, errors.New("Push: pre-receive hook declined")) {
		t.Fatal("a refusal that coincided with a cancellation must still be reported")
	}
}
