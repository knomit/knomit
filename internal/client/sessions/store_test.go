package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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
	rows, _, err := s.List(ctx, Filter{Now: t0.Add(time.Minute)})
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
	rows, _, _ := s.List(ctx, Filter{Now: t0, IncludeHidden: true})
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
	rows, _, _ := s.List(ctx, Filter{Now: t0})
	if rows[0].Transport != "http" || rows[0].InstanceID != DeriveInstanceID("10.0.0.5", "curl/8", "", "") {
		t.Fatalf("%+v", rows[0])
	}
	if err := s.SetClientInfo(ctx, "sid", "lens:l1", "mcp-inspector", "0.9", "10.0.0.5", "curl/8", t0); err != nil {
		t.Fatal(err)
	}
	want := DeriveInstanceID("10.0.0.5", "curl/8", "mcp-inspector", "0.9")
	rows, _, _ = s.List(ctx, Filter{Now: t0})
	if rows[0].InstanceID != want || !rows[0].Initialized || rows[0].ClientName != "mcp-inspector" {
		t.Fatalf("%+v", rows[0])
	}
	// A later touch must not change it.
	obs.Now = t0.Add(time.Minute)
	if err := s.Touch(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.List(ctx, Filter{Now: t0.Add(time.Minute)})
	if rows[0].InstanceID != want {
		t.Fatalf("instance id drifted: %s", rows[0].InstanceID)
	}
}

func TestSetClientInfo_BeforeFirstTouchCreatesRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetClientInfo(ctx, "sid", "repo:uid1", "claude-code", "2.0", "127.0.0.1", "knomit-bridge/1.0", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, bridgeObs("sid", t0.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.List(ctx, Filter{Now: t0.Add(time.Second)})
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
	rows, _, _ := s.List(ctx, Filter{Now: t0.Add(2 * time.Second)})
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

	rows, _, err := s.List(ctx, Filter{Now: now})
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
	rows, _, _ = s.List(ctx, Filter{Now: now, IncludeHidden: true})
	if len(rows) != 5 {
		t.Fatalf("include hidden: %d", len(rows))
	}
	rows, _, _ = s.List(ctx, Filter{Now: now, Binding: "repo:uid2"})
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
	rows, _, _ := s.List(ctx, Filter{Now: now, IncludeHidden: true})
	if len(rows) != 2 {
		t.Fatalf("%d rows remain", len(rows))
	}
	s.policy.Retention = 0
	n, err = s.Purge(ctx, now.Add(1000*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("disabled purge: n=%d err=%v", n, err)
	}
}

// The session id is the one client-supplied value that becomes a PRIMARY KEY,
// so it is capped like every other: an unbounded header must not become an
// unbounded row key.
func TestSessionIDIsCapped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	long := strings.Repeat("x", 4000)

	if err := s.Touch(ctx, bridgeObs(long, t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(ctx, long, "repo:uid1", "n", "v", "1.2.3.4", "ua", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.End(ctx, long, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.List(ctx, Filter{Now: t0.Add(time.Second), IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("all three calls must address ONE capped row, got %d", len(rows))
	}
	if len(rows[0].ID) != MaxFieldLen+len(truncationSuffix) {
		t.Fatalf("session id not capped: len=%d", len(rows[0].ID))
	}
	if rows[0].Ended == nil {
		t.Fatal("End must reach the same capped row")
	}
}

// A DELETE is the client SAYING it is done, not proof that it is: with the
// default mcp-go manager the same id keeps working afterwards. A later
// request must bring the row back to life, or the row reads "ended" while
// the session is demonstrably still making calls.
func TestTouch_AfterEndRevivesTheRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		obs  func(now time.Time) Observation
	}{
		{"declared bridge", func(now time.Time) Observation { return bridgeObs("sid", now) }},
		{"direct http", func(now time.Time) Observation {
			return Observation{SessionID: "sid", Binding: "repo:uid1", RemoteIP: "10.0.0.5", UserAgent: "curl/8", Now: now}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := s
			if tc.name == "direct http" {
				s = newTestStore(t)
			}
			if err := s.Touch(ctx, tc.obs(t0)); err != nil {
				t.Fatal(err)
			}
			if err := s.End(ctx, "sid", t0.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := s.Touch(ctx, tc.obs(t0.Add(2*time.Second))); err != nil {
				t.Fatal(err)
			}

			rows, _, err := s.List(ctx, Filter{Now: t0.Add(2 * time.Second)})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows=%d", len(rows))
			}
			if rows[0].Ended != nil {
				t.Fatalf("a request after the DELETE must clear ended_at: %+v", rows[0])
			}
			if rows[0].State != StateLive {
				t.Fatalf("state=%s, want live", rows[0].State)
			}
		})
	}
}

// The initialize hook is the FIRST thing to touch a direct-HTTP session's
// row, before any Touch, so it must derive the identity from what the server
// observed on THAT request. Re-reading the row it just created yields ” for
// both ip and User-Agent, which collapses every client declaring the same
// clientInfo onto one instance id.
func TestSetClientInfo_DerivesHTTPIdentityFromTheRequest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.SetClientInfo(ctx, "s1", "repo:uid1", "mcp-inspector", "0.9", "198.51.100.1", "curl/8", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(ctx, "s2", "repo:uid1", "mcp-inspector", "0.9", "203.0.113.2", "curl/8", t0); err != nil {
		t.Fatal(err)
	}

	rows, _, err := s.List(ctx, Filter{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	byID := map[string]Session{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if byID["s1"].InstanceID == byID["s2"].InstanceID {
		t.Fatalf("same clientInfo from different IPs collapsed onto one instance id: %s", byID["s1"].InstanceID)
	}
	if byID["s1"].InstanceID != DeriveInstanceID("198.51.100.1", "curl/8", "mcp-inspector", "0.9") {
		t.Fatalf("s1 instance id not derived from the request: %s", byID["s1"].InstanceID)
	}
	if byID["s1"].RemoteAddr != "198.51.100.1" || byID["s1"].UserAgent != "curl/8" {
		t.Fatalf("observed fields not recorded: %+v", byID["s1"])
	}
	// And it must agree with what a later Touch derives, so the id is stable.
	obs := Observation{SessionID: "s1", Binding: "repo:uid1", RemoteIP: "198.51.100.1", UserAgent: "curl/8", Now: t0.Add(time.Minute)}
	if err := s.Touch(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.List(ctx, Filter{Now: t0.Add(time.Minute)})
	for _, r := range rows {
		if r.ID == "s1" && r.InstanceID != DeriveInstanceID("198.51.100.1", "curl/8", "mcp-inspector", "0.9") {
			t.Fatalf("instance id drifted on the first Touch: %s", r.InstanceID)
		}
	}
}

// A declared (stdio) row must keep the identity the bridge declared — the
// hook must not overwrite it with a server-derived one.
func TestSetClientInfo_DoesNotClobberDeclaredIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Touch(ctx, bridgeObs("sid", t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClientInfo(ctx, "sid", "repo:uid1", "claude-code", "2.0", "198.51.100.1", "knomit-bridge/1", t0); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.List(ctx, Filter{Now: t0})
	if rows[0].InstanceID != "inst1" || rows[0].Transport != "stdio" {
		t.Fatalf("declared identity overwritten: %+v", rows[0])
	}
}

// TRUNCATION KEEPS THE NEWEST ROWS, IN ORDER. A count-only assertion would pass
// against a LIMIT that kept an arbitrary page, which for a presence view is the
// wrong page: the sessions worth showing are the most recently active.
func TestList_TruncationKeepsNewestInOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Distinguishable last_seen times, seeded OLDEST first so insertion order
	// cannot be what makes the assertion pass.
	const n = 12
	for i := 0; i < n; i++ {
		o := bridgeObs(fmt.Sprintf("s%02d", i), t0.Add(time.Duration(i)*time.Minute))
		if err := s.Touch(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	rows, truncated, err := s.List(ctx, Filter{Now: t0.Add(time.Hour), IncludeHidden: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("more rows matched than were returned; truncated must be true")
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want exactly the limit", len(rows))
	}
	// last_seen_at DESC: the five newest are s11..s07, in that order.
	want := []string{"s11", "s10", "s09", "s08", "s07"}
	for i, id := range want {
		if rows[i].ID != id {
			got := make([]string, len(rows))
			for j, r := range rows {
				got[j] = r.ID
			}
			t.Fatalf("row %d is %q, want %q (full page %v, want %v)", i, rows[i].ID, id, got, want)
		}
	}
}

// truncated distinguishes a full page from an exhausted one. Both return the
// same number of rows, so nothing but this flag can tell them apart.
func TestList_TruncatedIsFalseOnExactFit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = s.Touch(ctx, bridgeObs(fmt.Sprintf("s%d", i), t0.Add(time.Duration(i)*time.Minute)))
	}

	// Exactly the limit: full page, nothing beyond it.
	rows, truncated, err := s.List(ctx, Filter{Now: t0.Add(time.Hour), IncludeHidden: true, Limit: 5})
	if err != nil || len(rows) != 5 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if truncated {
		t.Fatal("an exactly-full page is not truncated — the probe row must not be counted")
	}
	// One fewer than the limit: also not truncated.
	if _, truncated, _ := s.List(ctx, Filter{Now: t0.Add(time.Hour), IncludeHidden: true, Limit: 6}); truncated {
		t.Fatal("an under-full page is not truncated")
	}
}

// The limit has a default and a ceiling, and there is no way to ask for the
// unbounded read back.
func TestResolveListLimit(t *testing.T) {
	for in, want := range map[int]int{
		0:                DefaultListLimit,
		-1:               DefaultListLimit,
		1:                1,
		DefaultListLimit: DefaultListLimit,
		MaxListLimit:     MaxListLimit,
		MaxListLimit + 1: MaxListLimit,
		1 << 30:          MaxListLimit,
	} {
		if got := ResolveListLimit(in); got != want {
			t.Errorf("ResolveListLimit(%d)=%d, want %d", in, got, want)
		}
	}
}

// The kernel's pid and the client's own declared pid are BOTH kept, and they
// are allowed to differ: the declared one is a header the client wrote about
// itself, the verified one is what the kernel said. Collapsing them would
// throw away the only evidence a mismatch ever leaves.
func TestTouch_RecordsPrincipalAndVerifiedPID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	o := bridgeObs("sid-1", t0)
	o.Client.PID = 999 // self-declared, deliberately wrong
	o.Principal = "bridge:uid:501@socket"
	o.VerifiedPID = 4242
	if err := s.Touch(ctx, o); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.List(ctx, Filter{Now: t0})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].Principal != "bridge:uid:501@socket" {
		t.Fatalf("principal = %q", rows[0].Principal)
	}
	if rows[0].VerifiedPID != 4242 || rows[0].PID != 999 {
		t.Fatalf("verified=%d declared=%d: both must survive and differ", rows[0].VerifiedPID, rows[0].PID)
	}
}

// A TCP caller has no peer, so it has no verified pid and no principal row;
// absence must read as 0/"" rather than as a scan error.
func TestTouch_NoPeerLeavesVerifiedPIDZero(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Touch(ctx, bridgeObs("sid-tcp", t0)); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.List(ctx, Filter{Now: t0})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].VerifiedPID != 0 || rows[0].Principal != "" {
		t.Fatalf("a TCP session must carry neither: %+v", rows[0])
	}
}

// A later request that carries no principal must not ERASE one already
// recorded: the bridge's very first request can precede the peer being
// resolved, and a blank overwrite would lose the verified identity.
func TestTouch_BlankPrincipalDoesNotEraseARecordedOne(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	o := bridgeObs("sid-1", t0)
	o.Principal = "bridge:uid:501@socket"
	o.VerifiedPID = 4242
	if err := s.Touch(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch(ctx, bridgeObs("sid-1", t0.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := s.List(ctx, Filter{Now: t0})
	if rows[0].Principal != "bridge:uid:501@socket" || rows[0].VerifiedPID != 4242 {
		t.Fatalf("a blank observation erased the verified identity: %+v", rows[0])
	}
}

// client_session_peers has no foreign key, so Purge has to collect its
// orphans the way it collects client_session_bindings'.
func TestPurge_CollectsPeerOrphans(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	o := bridgeObs("sid-old", t0)
	o.Principal = "bridge:uid:501@socket"
	o.VerifiedPID = 4242
	if err := s.Touch(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge(ctx, t0.Add(200*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_session_peers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("client_session_peers rows left after purge: %d", n)
	}
}
