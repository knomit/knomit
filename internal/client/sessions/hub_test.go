package sessions

import (
	"context"
	"testing"
	"time"
)

// newHubStore is newTestStore with a change hub attached, which is how the
// server builds it (internal/repos/manager.go). The bare newTestStore stays
// hubless on purpose: every existing test in this package proves that writes
// do not depend on a hub being there.
func newHubStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	return newTestStore(t).WithHub(ctx)
}

func TestStore_PublishesChangePerWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newHubStore(t, ctx)
	events := s.Subscribe(ctx)

	next := func(what string) Change {
		t.Helper()
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatalf("%s: stream closed", what)
			}
			c, ok := e.(Change)
			if !ok {
				t.Fatalf("%s: got %T on the stream, want Change", what, e)
			}
			return c
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no Change published", what)
			return Change{}
		}
	}

	if err := s.Touch(ctx, bridgeObs("sid", t0)); err != nil {
		t.Fatal(err)
	}
	if c := next("Touch"); c.ID != "sid" || c.Kind != "touch" {
		t.Errorf("Touch published %+v, want {sid touch}", c)
	}

	if err := s.SetClientInfo(ctx, "sid", "repo:uid1", "claude-code", "2.0", "198.51.100.1", "ua", t0); err != nil {
		t.Fatal(err)
	}
	if c := next("SetClientInfo"); c.ID != "sid" || c.Kind != "init" {
		t.Errorf("SetClientInfo published %+v, want {sid init}", c)
	}

	if err := s.End(ctx, "sid", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if c := next("End"); c.ID != "sid" || c.Kind != "end" {
		t.Errorf("End published %+v, want {sid end}", c)
	}
}

// A write that changed nothing must not wake every connected browser: the
// no-op guards return before the publish, not after it.
func TestStore_NoChangeWithoutAWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newHubStore(t, ctx)
	events := s.Subscribe(ctx)

	// Empty session id is a no-op on all three writers.
	if err := s.Touch(ctx, Observation{SessionID: "", Now: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(ctx, "", "", "n", "v", "ip", "ua", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, "", t0); err != nil {
		t.Fatal(err)
	}
	// Purge with nothing old enough to delete deletes no rows.
	if n, err := s.Purge(ctx, t0); err != nil || n != 0 {
		t.Fatalf("Purge: %d, %v; want 0, nil", n, err)
	}
	// An End for a session this server never recorded is documented as a
	// no-op, so it must not ping either.
	if err := s.End(ctx, "never-seen", t0); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-events:
		t.Fatalf("published %+v for writes that changed nothing", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// Purge is the one change that names no single row: the UI re-reads the whole
// list, so an empty id is the honest payload.
func TestStore_PurgePublishesRowlessChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newHubStore(t, ctx)
	if err := s.Touch(ctx, bridgeObs("old", t0)); err != nil {
		t.Fatal(err)
	}
	events := s.Subscribe(ctx)

	n, err := s.Purge(ctx, t0.Add(200*time.Hour)) // retention is 168h in the test policy
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Purge deleted %d rows, want 1", n)
	}
	select {
	case e := <-events:
		c, ok := e.(Change)
		if !ok {
			t.Fatalf("got %T, want Change", e)
		}
		if c.Kind != "purge" || c.ID != "" {
			t.Errorf("Purge published %+v, want {\"\" purge}", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Purge published no Change")
	}
}

// A hubless Store — every existing test, and any caller that never wires one —
// must keep writing. Subscribe on it yields a channel that never delivers, so
// a stream over such a store degrades to keepalives instead of closing and
// sending the browser into a reconnect loop.
func TestStore_WithoutHub_WritesSucceedAndSubscribeNeverDelivers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Touch(ctx, bridgeObs("sid", t0)); err != nil {
		t.Fatalf("Touch on a hubless store: %v", err)
	}
	rows, err := s.List(ctx, Filter{Now: t0})
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: %d rows, %v; want 1, nil", len(rows), err)
	}
	select {
	case e, ok := <-s.Subscribe(ctx):
		t.Fatalf("hubless Subscribe delivered %+v (open=%v); want a channel that never delivers", e, ok)
	case <-time.After(100 * time.Millisecond):
	}
}
