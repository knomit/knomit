package manage

import (
	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"testing"
)

// THE WIRE CONTRACT FOR pct: present when there is a percentage, ABSENT when
// there is not. Not zero — the job's pct is a latest value, so a zero here
// would run 5 → 0 → 70 across a create and send a monotonic client's bar
// backwards mid-transfer.
//
// This is the server half of the rule the UI honours; PendingCreateRow's
// vitest is the client half. The two are pinned separately on purpose: they
// are what a THIRD client would read, and a body that kept sending 40 while
// saying "indeterminate" would draw the incident's frozen bar in anything that
// ignored the flag.
func TestCreateStatusBody_OmitsPctWhenIndeterminate(t *testing.T) {
	b := hal.URLBuilder{Base: "/api/v1"}

	transfer := createStatusBody(b, repos.CreateStatus{
		ID: "c1", Name: "kb", Mode: "subscribe", State: repos.CreateRunning,
		Step: "subscribe", Phase: repos.PhaseTransfer, Indeterminate: true,
		Message: "knomit: sent 3 MiB", Pct: 40,
	})
	if _, ok := transfer["pct"]; ok {
		t.Fatalf("an indeterminate status must carry NO pct, got %v", transfer["pct"])
	}
	if transfer["indeterminate"] != true {
		t.Fatalf("indeterminate must be true: %v", transfer)
	}

	// And the other half of the contract: a determinate status DOES carry it,
	// or "absent" would mean nothing.
	index := createStatusBody(b, repos.CreateStatus{
		ID: "c1", Name: "kb", Mode: "subscribe", State: repos.CreateRunning,
		Step: "index", Phase: repos.PhaseIndex, Message: "indexing 40/60", Pct: 97,
	})
	if index["pct"] != 97 {
		t.Fatalf("a determinate status must carry its pct, got %v", index["pct"])
	}
	if index["indeterminate"] != false {
		t.Fatalf("indeterminate must be false: %v", index)
	}
}
