package sessions

import (
	"context"
	"testing"
	"time"
)

func TestBindSession_SetThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, ok, err := s.SessionBinding(ctx, "sid"); err != nil || ok {
		t.Fatalf("unbound: ok=%v err=%v", ok, err)
	}
	if err := s.BindSession(ctx, "sid", "repo:u1", t0); err != nil {
		t.Fatal(err)
	}
	pin, ok, err := s.SessionBinding(ctx, "sid")
	if err != nil || !ok || pin != "repo:u1" {
		t.Fatalf("pin=%q ok=%v err=%v", pin, ok, err)
	}
}

func TestBindSession_RebindOverwrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.BindSession(ctx, "sid", "repo:u1", t0)
	if err := s.BindSession(ctx, "sid", "lens:l1", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	pin, _, _ := s.SessionBinding(ctx, "sid")
	if pin != "lens:l1" {
		t.Fatalf("pin=%q", pin)
	}
}

func TestBindSession_EmptyArgsRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.BindSession(context.Background(), "", "repo:u1", t0); err == nil {
		t.Fatal("empty sid accepted")
	}
	if err := s.BindSession(context.Background(), "sid", "", t0); err == nil {
		t.Fatal("empty pin accepted")
	}
}

func TestPurge_DropsOrphanedBindings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Touch(ctx, bridgeObs("old", t0))
	_ = s.BindSession(ctx, "old", "repo:u1", t0)
	_ = s.Touch(ctx, bridgeObs("fresh", t0.Add(200*time.Hour)))
	_ = s.BindSession(ctx, "fresh", "repo:u1", t0.Add(200*time.Hour))
	if _, err := s.Purge(ctx, t0.Add(200*time.Hour)); err != nil { // retention 168h
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionBinding(ctx, "old"); ok {
		t.Fatal("orphaned binding survived purge")
	}
	if _, ok, _ := s.SessionBinding(ctx, "fresh"); !ok {
		t.Fatal("live binding purged")
	}
}
