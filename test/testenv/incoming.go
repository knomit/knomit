package testenv

import (
	"context"
	"testing"

	"knomit/internal/store"
)

// Incoming returns the incoming-edge view at this fact handle's commit pin.
// Receiver must be in FactStateExists (enforced via MustExist).
//
// Mirrors the production read path: it calls store.GraphStore.IncomingAtCommit
// directly, so the result is identical to what the HTTP /incoming endpoint
// would return — no HTTP round-trip in tests.
func (f *FactHandle) Incoming() *EdgeView {
	f.MustExist()
	t := f.t
	t.Helper()

	var refs []store.RefSummary
	var err error
	f.branch.repo.ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		refs, err = svc.GraphStore().IncomingAtCommit(context.Background(), f.branch.name, f.path, f.commit)
	})
	if err != nil {
		t.Fatalf("Incoming(%s @ %s): %v", f.path, f.commit, err)
	}
	return &EdgeView{t: t, kind: "incoming", path: f.path, commit: f.commit, items: refs}
}

// Outgoing returns the outgoing-edge view at this fact handle's commit pin.
func (f *FactHandle) Outgoing() *EdgeView {
	f.MustExist()
	t := f.t
	t.Helper()

	var refs []store.RefSummary
	var err error
	f.branch.repo.ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		refs, err = svc.Search().OutgoingAtCommit(context.Background(), f.branch.name, f.path, f.commit)
	})
	if err != nil {
		t.Fatalf("Outgoing(%s @ %s): %v", f.path, f.commit, err)
	}
	return &EdgeView{t: t, kind: "outgoing", path: f.path, commit: f.commit, items: refs}
}

// EdgeView wraps a slice of RefSummary with assertion helpers. Used for
// both /incoming and /outgoing collections (they share a shape).
type EdgeView struct {
	t      *testing.T
	kind   string // "incoming" or "outgoing", for error messages
	path   string
	commit string
	items  []store.RefSummary
}

// MustHaveCount asserts the view has exactly n entries.
func (v *EdgeView) MustHaveCount(n int) *EdgeView {
	v.t.Helper()
	if len(v.items) != n {
		v.t.Fatalf("%s for %s @ %s: expected %d entries, got %d: %+v",
			v.kind, v.path, v.commit, n, len(v.items), v.items)
	}
	return v
}

// MustHaveItem asserts the view contains an entry matching (path, commit).
func (v *EdgeView) MustHaveItem(path, commit string) *EdgeView {
	v.t.Helper()
	for _, r := range v.items {
		if r.Path == path && r.Commit == commit {
			return v
		}
	}
	v.t.Fatalf("%s for %s @ %s: missing (%s, %s); got %+v",
		v.kind, v.path, v.commit, path, commit, v.items)
	return v
}

// MustHaveOnly asserts exactly one entry, matching (path, commit).
func (v *EdgeView) MustHaveOnly(path, commit string) *EdgeView {
	v.t.Helper()
	v.MustHaveCount(1)
	v.MustHaveItem(path, commit)
	return v
}
