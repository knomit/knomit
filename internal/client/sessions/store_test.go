package sessions

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	storemigrate "knomit/internal/store/migrate"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // same as the real registry handle
	t.Cleanup(func() { db.Close() })
	if err := storemigrate.Control(db); err != nil {
		t.Fatal(err)
	}
	return New(db, Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour, Retention: 168 * time.Hour})
}

var t0 = time.Unix(1_700_000_000, 0)

func bridgeObs(id string, now time.Time) Observation {
	return Observation{
		SessionID: id, Binding: "repo:uid1", RemoteIP: "127.0.0.1", UserAgent: "knomit-bridge/1.0", Now: now,
		Client: &BridgeInfo{InstanceID: "inst1", Transport: "stdio", PID: 42, ParentPID: 41, ParentApp: "claude",
			Host: "h", User: "u", Cwd: "/w", Branch: "agent/x", Version: "1.0"},
	}
}

func TestTouch_InsertsThenBumps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Touch(ctx, bridgeObs("sid", t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, bridgeObs("sid", t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, Filter{Now: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.RequestCount != 2 || !r.FirstSeen.Equal(t0) || !r.LastSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", r)
	}
	if r.InstanceID != "inst1" || r.Transport != "stdio" || r.PID != 42 || r.Cwd != "/w" || r.State != StateLive {
		t.Fatalf("%+v", r)
	}
}

func TestTouch_EmptySessionIDIsNoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Touch(ctx, bridgeObs("", t0)); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, Filter{Now: t0, IncludeHidden: true})
	if len(rows) != 0 {
		t.Fatalf("a request without a session id must record nothing, got %d rows", len(rows))
	}
}

func TestTouch_HTTPIdentityDerivedAndStableAfterClientInfo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	obs := Observation{SessionID: "sid", Binding: "lens:l1", RemoteIP: "10.0.0.5", UserAgent: "curl/8", Now: t0}
	if err := s.Touch(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, Filter{Now: t0})
	if rows[0].Transport != "http" || rows[0].InstanceID != DeriveInstanceID("10.0.0.5", "curl/8", "", "") {
		t.Fatalf("%+v", rows[0])
	}
	if err := s.SetClientInfo(ctx, "sid", "lens:l1", "mcp-inspector", "0.9", t0); err != nil {
		t.Fatal(err)
	}
	want := DeriveInstanceID("10.0.0.5", "curl/8", "mcp-inspector", "0.9")
	rows, _ = s.List(ctx, Filter{Now: t0})
	if rows[0].InstanceID != want || !rows[0].Initialized || rows[0].ClientName != "mcp-inspector" {
		t.Fatalf("%+v", rows[0])
	}
	// A later touch must not change it.
	obs.Now = t0.Add(time.Minute)
	if err := s.Touch(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.List(ctx, Filter{Now: t0.Add(time.Minute)})
	if rows[0].InstanceID != want {
		t.Fatalf("instance id drifted: %s", rows[0].InstanceID)
	}
}

func TestSetClientInfo_BeforeFirstTouchCreatesRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetClientInfo(ctx, "sid", "repo:uid1", "claude-code", "2.0", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, bridgeObs("sid", t0.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, Filter{Now: t0.Add(time.Second)})
	r := rows[0]
	if r.ClientName != "claude-code" || !r.Initialized || r.Transport != "stdio" || r.InstanceID != "inst1" || r.RequestCount != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestEnd_MarksDead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Touch(ctx, bridgeObs("sid", t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, "sid", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.List(ctx, Filter{Now: t0.Add(2 * time.Second)})
	if rows[0].State != StateDead || rows[0].Ended == nil {
		t.Fatalf("%+v", rows[0])
	}
	if err := s.End(ctx, "unknown", t0); err != nil {
		t.Fatalf("End on unknown id must be a no-op, got %v", err)
	}
}

func TestList_StatesFilterAndHidden(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := t0.Add(10 * time.Hour)
	_ = s.Touch(ctx, bridgeObs("live", now.Add(-time.Minute)))
	_ = s.Touch(ctx, bridgeObs("idle", now.Add(-30*time.Minute)))
	_ = s.Touch(ctx, bridgeObs("dead", now.Add(-2*time.Hour)))
	_ = s.Touch(ctx, bridgeObs("hidden", now.Add(-4*time.Hour)))
	other := bridgeObs("other", now)
	other.Binding = "repo:uid2"
	_ = s.Touch(ctx, other)

	rows, err := s.List(ctx, Filter{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]State{}
	for _, r := range rows {
		got[r.ID] = r.State
	}
	if len(rows) != 4 || got["live"] != StateLive || got["idle"] != StateIdle || got["dead"] != StateDead {
		t.Fatalf("%v", got)
	}
	if _, ok := got["hidden"]; ok {
		t.Fatal("hidden row must be excluded by default")
	}
	if rows[0].ID != "other" { // newest last_seen first
		t.Fatalf("order: %s", rows[0].ID)
	}
	rows, _ = s.List(ctx, Filter{Now: now, IncludeHidden: true})
	if len(rows) != 5 {
		t.Fatalf("include hidden: %d", len(rows))
	}
	rows, _ = s.List(ctx, Filter{Now: now, Binding: "repo:uid2"})
	if len(rows) != 1 || rows[0].ID != "other" {
		t.Fatalf("binding filter: %+v", rows)
	}
}

func TestPurge_RetentionBoundaryAndDisabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := t0.Add(400 * time.Hour)
	_ = s.Touch(ctx, bridgeObs("old", now.Add(-169*time.Hour)))
	_ = s.Touch(ctx, bridgeObs("edge", now.Add(-168*time.Hour)))
	_ = s.Touch(ctx, bridgeObs("new", now))
	n, err := s.Purge(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	rows, _ := s.List(ctx, Filter{Now: now, IncludeHidden: true})
	if len(rows) != 2 {
		t.Fatalf("%d rows remain", len(rows))
	}
	s.policy.Retention = 0
	n, err = s.Purge(ctx, now.Add(1000*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("disabled purge: n=%d err=%v", n, err)
	}
}
