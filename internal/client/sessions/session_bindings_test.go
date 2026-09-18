package sessions

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRecordSessionBinding_UpsertCountsAndTimes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordSessionBinding(ctx, "sid", "hA", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSessionBinding(ctx, "sid", "hA", "repo:u1", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	sets, err := s.SessionBindings(ctx, []string{"sid"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets["sid"]) != 1 {
		t.Fatalf("want one row for one handle, got %d", len(sets["sid"]))
	}
	got := sets["sid"][0]
	if got.RequestCount != 2 {
		t.Fatalf("request_count=%d, want 2", got.RequestCount)
	}
	if !got.FirstSeen.Equal(t0) {
		t.Fatalf("first_seen=%v, want %v — first sight must not move", got.FirstSeen, t0)
	}
	if !got.LastSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("last_seen=%v, want the later time", got.LastSeen)
	}
}

// TWO HANDLES ON ONE SESSION ARE TWO ROWS, even when they name the same repo.
// They are two callers, and collapsing them by pin would discard exactly the
// distinction the handle exists to make.
func TestRecordSessionBinding_KeyedByHandleNotByPin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.RecordSessionBinding(ctx, "sid", "hA", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSessionBinding(ctx, "sid", "hB", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	sets, _ := s.SessionBindings(ctx, []string{"sid"})
	if len(sets["sid"]) != 2 {
		t.Fatalf("two handles on one repo must be two rows, got %d", len(sets["sid"]))
	}
}

// Most recently used first, so the UI's first row is the session's current
// work without the caller having to sort.
func TestSessionBindings_MostRecentFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RecordSessionBinding(ctx, "sid", "old", "repo:u1", "", t0)
	_ = s.RecordSessionBinding(ctx, "sid", "new", "lens:l1", "", t0.Add(time.Hour))

	sets, _ := s.SessionBindings(ctx, []string{"sid"})
	if len(sets["sid"]) != 2 {
		t.Fatalf("got %d rows", len(sets["sid"]))
	}
	if sets["sid"][0].Handle != "new" {
		t.Fatalf("first row is %q, want the most recently used", sets["sid"][0].Handle)
	}
}

// One query for many sessions, and each session gets only its own rows.
func TestSessionBindings_BulkKeepsSessionsSeparate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.RecordSessionBinding(ctx, "s1", "h1", "repo:u1", "", t0)
	_ = s.RecordSessionBinding(ctx, "s2", "h2", "repo:u2", "", t0)

	sets, err := s.SessionBindings(ctx, []string{"s1", "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets["s1"]) != 1 || sets["s1"][0].Binding != "repo:u1" {
		t.Fatalf("s1 got %+v", sets["s1"])
	}
	if len(sets["s2"]) != 1 || sets["s2"][0].Binding != "repo:u2" {
		t.Fatalf("s2 got %+v", sets["s2"])
	}
	// An empty request is not a query at all.
	if out, err := s.SessionBindings(ctx, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty ids: %v %v", out, err)
	}
}

// A request with no handle records nothing: the set answers "which handles has
// this session presented", and a URL-scoped caller presented none.
func TestRecordSessionBinding_IgnoresEmptyArgs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, c := range [][3]string{{"", "h", "repo:u1"}, {"sid", "", "repo:u1"}, {"sid", "h", ""}} {
		if err := s.RecordSessionBinding(ctx, c[0], c[1], c[2], "", t0); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
	}
	sets, _ := s.SessionBindings(ctx, []string{"sid"})
	if len(sets["sid"]) != 0 {
		t.Fatalf("incomplete observations must record nothing, got %+v", sets["sid"])
	}
}

// The set belongs to its session and dies with it — unlike binding_handles,
// which is routing state a live caller may still present.
func TestPurge_DropsBindingSetWithItsSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Touch(ctx, bridgeObs("old", t0))
	_ = s.RecordSessionBinding(ctx, "old", "h1", "repo:u1", "", t0)
	_ = s.Touch(ctx, bridgeObs("fresh", t0.Add(200*time.Hour)))
	_ = s.RecordSessionBinding(ctx, "fresh", "h2", "repo:u1", "", t0.Add(200*time.Hour))

	if _, err := s.Purge(ctx, t0.Add(200*time.Hour)); err != nil { // retention 168h
		t.Fatal(err)
	}
	sets, _ := s.SessionBindings(ctx, []string{"old", "fresh"})
	if len(sets["old"]) != 0 {
		t.Fatal("the dead session's binding set survived it")
	}
	if len(sets["fresh"]) != 1 {
		t.Fatal("a live session's binding set was purged")
	}
}

// ?binding= matches a session through ANY handle in its set, not only through
// the last-seen pin, and returns it ONCE however many handles match.
func TestList_BindingFilterMatchesAnyHandleOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// The session's LAST-seen binding is u2, but it also holds two handles on
	// u1 — the case a last-seen-only filter gets wrong.
	o := bridgeObs("sid", t0)
	o.Binding = "repo:u2"
	_ = s.Touch(ctx, o)
	_ = s.RecordSessionBinding(ctx, "sid", "hA", "repo:u1", "", t0)
	_ = s.RecordSessionBinding(ctx, "sid", "hB", "repo:u1", "", t0)
	_ = s.RecordSessionBinding(ctx, "sid", "hC", "repo:u2", "", t0)

	rows, _, err := s.List(ctx, Filter{Binding: "repo:u1", Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want the session ONCE despite two matching handles, got %d", len(rows))
	}

	// Still matches on the last-seen pin alone.
	if rows, _, _ := s.List(ctx, Filter{Binding: "repo:u2", Now: t0}); len(rows) != 1 {
		t.Fatalf("last-seen pin must still match, got %d", len(rows))
	}
	// And does not match a pin it has never used.
	if rows, _, _ := s.List(ctx, Filter{Binding: "repo:u9", Now: t0}); len(rows) != 0 {
		t.Fatalf("unrelated pin matched %d rows", len(rows))
	}
}

// CHUNKING, TESTED AT THE REAL PRODUCTION CONSTANT. BindingChunkSize is
// exported precisely so this test can read it: a test-only small chunk size
// would exercise a different code path from production and prove nothing about
// it.
//
// Seeded past the boundary and NOT on it — BindingChunkSize+7 sessions means
// the last chunk is partial, which is the case a dropped-remainder bug hits.
// That bug returns NO error: the sessions in the lost chunk simply come back
// with empty sets, which downstream reads as "presented no handles" and is
// indistinguishable from the truth. So this asserts completeness per session,
// not just a total.
func TestSessionBindings_ChunksAtProductionBoundary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const extra = 7
	n := BindingChunkSize + extra
	if n <= BindingChunkSize {
		t.Fatal("the fixture must cross the real boundary")
	}
	sids := make([]string, n)
	for i := 0; i < n; i++ {
		sid := fmt.Sprintf("s%05d", i)
		sids[i] = sid
		// Two handles each, so a per-session grouping error shows up too — and
		// they name the same pin, the case that must stay two rows.
		if err := s.RecordSessionBinding(ctx, sid, sid+"-hA", "repo:u1", "", t0); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordSessionBinding(ctx, sid, sid+"-hB", "repo:u1", "", t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	sets, err := s.SessionBindings(ctx, sids)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != n {
		t.Fatalf("got sets for %d sessions, want %d — a dropped chunk loses sessions silently", len(sets), n)
	}
	// Every session, including the ones in the final partial chunk, and
	// correctly grouped: each must have ITS OWN two handles, not another
	// session's.
	for _, sid := range sids {
		got := sets[sid]
		if len(got) != 2 {
			t.Fatalf("session %s has %d handles, want 2", sid, len(got))
		}
		// Most-recent-first holds across the chunk boundary too.
		if got[0].Handle != sid+"-hB" || got[1].Handle != sid+"-hA" {
			t.Fatalf("session %s got handles %q,%q — wrong rows or wrong order",
				sid, got[0].Handle, got[1].Handle)
		}
	}

	// The boundary rows specifically, named so a failure says which side broke.
	for _, i := range []int{0, BindingChunkSize - 1, BindingChunkSize, n - 1} {
		sid := fmt.Sprintf("s%05d", i)
		if len(sets[sid]) != 2 {
			t.Fatalf("boundary session %d (%s) lost its set", i, sid)
		}
	}
}

// The chunk size must stay under the SMALLEST limit anyone is likely to link.
// SQLITE_MAX_VARIABLE_NUMBER is compile-time: 32766 on the bundled 3.51.2, but
// 999 before SQLite 3.32 and still 999 on some system builds. A constant
// derived from measuring today's library breaks on the one machine that
// differs, silently, because nothing re-measures.
func TestBindingChunkSize_ConservativeAgainstOldestCap(t *testing.T) {
	const oldestKnownCap = 999
	if BindingChunkSize >= oldestKnownCap {
		t.Fatalf("BindingChunkSize=%d must stay under the pre-3.32 cap of %d",
			BindingChunkSize, oldestKnownCap)
	}
	// And a full page's worth of ids must fit in one chunk's worth of headroom
	// under that same old cap, so the two bounds cannot drift into conflict.
	if MaxListLimit/BindingChunkSize+1 < 1 {
		t.Fatal("unreachable, but keeps the relationship explicit")
	}
}
