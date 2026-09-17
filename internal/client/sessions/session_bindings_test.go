package sessions

import (
	"context"
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

	rows, err := s.List(ctx, Filter{Binding: "repo:u1", Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want the session ONCE despite two matching handles, got %d", len(rows))
	}

	// Still matches on the last-seen pin alone.
	if rows, _ := s.List(ctx, Filter{Binding: "repo:u2", Now: t0}); len(rows) != 1 {
		t.Fatalf("last-seen pin must still match, got %d", len(rows))
	}
	// And does not match a pin it has never used.
	if rows, _ := s.List(ctx, Filter{Binding: "repo:u9", Now: t0}); len(rows) != 0 {
		t.Fatalf("unrelated pin matched %d rows", len(rows))
	}
}
