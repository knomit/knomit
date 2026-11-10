package oauth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
)

// newPairingIssuer is an issuer over a credential store (design lifetimes,
// seeded bindings, settable clock). It has no issuer URL: the pairing core
// works whether or not the OAuth listener is configured.
func newPairingIssuer(t *testing.T) (*Issuer, *Store, *clock) {
	t.Helper()
	s, _, c := newCredStore(t)
	iss := NewIssuer(Options{
		Store:       s,
		Clients:     NewResolver(config.Defaults().OAuth.EffectiveClients()),
		Grants:      auth.NewSQLGrants(s.db),
		WaitTimeout: time.Second,
	})
	return iss, s, c
}

var codeRE = regexp.MustCompile(`^[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{3}-[ABCDEFGHJKLMNPQRSTUVWXYZ23456789]{3}$`)

func TestPairing_CodeShape(t *testing.T) {
	iss, _, _ := newPairingIssuer(t)
	for range 20 {
		id, code, err := iss.StartPairing(context.Background(), "Firefox", "10.0.0.7")
		if err != nil {
			t.Fatal(err)
		}
		if !codeRE.MatchString(code) || !pendingIDRE.MatchString(id) {
			t.Fatalf("id %q code %q", id, code)
		}
	}
}

func TestPairing_ApproveByCodeOrID(t *testing.T) {
	ctx := context.Background()
	iss, s, _ := newPairingIssuer(t)
	by := owner(t)

	id1, code1, err := iss.StartPairing(ctx, "Firefox", "10.0.0.7")
	if err != nil {
		t.Fatal(err)
	}
	// By code, typed lower-case and without the dash.
	if err := iss.ApprovePairing(ctx, strings.ToLower(strings.ReplaceAll(code1, "-", "")), by); err != nil {
		t.Fatalf("approve by code: %v", err)
	}
	cookie, err := iss.CollectPairing(ctx, id1)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.LookupSession(ctx, cookie)
	if err != nil || sess.Principal != by || sess.UserAgent != "Firefox" {
		t.Fatalf("session = %+v, %v; want the approver's session", sess, err)
	}

	id2, _, err := iss.StartPairing(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iss.ApprovePairing(ctx, id2, by); err != nil {
		t.Fatalf("approve by id: %v", err)
	}
	if err := iss.ApprovePairing(ctx, id2, by); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second approval: %v, want ErrNotPending", err)
	}
	for _, bad := range []string{"", "ZZZ-ZZZ", "not a code", strings.Repeat("A", 43)} {
		if err := iss.ApprovePairing(ctx, bad, by); !errors.Is(err, ErrUnknownPending) {
			t.Errorf("ApprovePairing(%q) = %v, want ErrUnknownPending", bad, err)
		}
	}
	// The approver must be a principal that can hold a session.
	id3, _, _ := iss.StartPairing(ctx, "", "")
	for _, p := range []authz.Principal{authz.Anonymous(), {Kind: authz.KindNode, ID: "fp"}, {}} {
		if err := iss.ApprovePairing(ctx, id3, p); err == nil {
			t.Errorf("approved by %+v", p)
		}
	}
}

func TestPairing_CollectOnce(t *testing.T) {
	ctx := context.Background()
	iss, _, _ := newPairingIssuer(t)
	id, code, _ := iss.StartPairing(ctx, "", "")
	if _, err := iss.CollectPairing(ctx, id); !errors.Is(err, ErrNotPending) {
		t.Fatalf("collect before approval: %v, want ErrNotPending", err)
	}
	if err := iss.ApprovePairing(ctx, code, owner(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.CollectPairing(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.CollectPairing(ctx, id); !errors.Is(err, ErrCollected) {
		t.Fatalf("second collect: %v, want ErrCollected", err)
	}
	if _, err := iss.CollectPairing(ctx, "nope"); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("unknown id: %v", err)
	}

	id2, code2, _ := iss.StartPairing(ctx, "", "")
	if err := iss.DenyPairing(ctx, code2, owner(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.CollectPairing(ctx, id2); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied: %v, want ErrDenied", err)
	}
}

// TestPairing_OwnCapDoesNotStarveAgentRequests: anonymous filling the pairing
// queue must not stop agent (OAuth client) requests, and vice versa.
func TestPairing_OwnCapDoesNotStarveAgentRequests(t *testing.T) {
	ctx := context.Background()
	iss, s, _ := newPairingIssuer(t)
	for i := range maxLivePairing {
		if _, _, err := iss.StartPairing(ctx, "", ""); err != nil {
			t.Fatalf("pairing %d: %v", i, err)
		}
	}
	if _, _, err := iss.StartPairing(ctx, "", ""); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("pairing beyond the cap: %v, want ErrTooManyPending", err)
	}
	agent := Pending{ClientID: "kb", RedirectURI: "http://127.0.0.1/callback", CodeChallenge: strings.Repeat("c", 43), Resource: "https://kb.example"}
	if _, err := s.CreatePending(ctx, agent); err != nil {
		t.Fatalf("agent request with the pairing queue full: %v", err)
	}
	// And the other way round: a full agent queue leaves pairing alone.
	for i := 1; i < maxLivePending; i++ {
		if _, err := s.CreatePending(ctx, agent); err != nil {
			t.Fatalf("agent %d: %v", i, err)
		}
	}
	if _, err := s.CreatePending(ctx, agent); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("agent beyond the cap: %v", err)
	}
}

func TestPairing_ExpiresAfterPendingTTL(t *testing.T) {
	ctx := context.Background()
	iss, _, c := newPairingIssuer(t)
	id, code, _ := iss.StartPairing(ctx, "", "")
	c.add(10 * time.Minute)
	if err := iss.ApprovePairing(ctx, code, owner(t)); !errors.Is(err, ErrExpired) && !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("approve after 10 min: %v, want expired", err)
	}
	if _, err := iss.CollectPairing(ctx, id); err == nil {
		t.Fatal("collected an expired pairing")
	}

	// Approved in time but collected too late: no session.
	id2, code2, _ := iss.StartPairing(ctx, "", "")
	if err := iss.ApprovePairing(ctx, code2, owner(t)); err != nil {
		t.Fatal(err)
	}
	c.add(10 * time.Minute)
	if _, err := iss.CollectPairing(ctx, id2); !errors.Is(err, ErrExpired) {
		t.Fatalf("collect after expiry: %v, want ErrExpired", err)
	}
}

// The old OAuth paths never see a pairing row: it is not listed, cannot be
// approved, denied or described as an agent request, and cannot be
// collected into an authorization code.
func TestPairing_InvisibleToTheAgentQueue(t *testing.T) {
	ctx := context.Background()
	iss, s, _ := newPairingIssuer(t)
	id, _, _ := iss.StartPairing(ctx, "", "")
	if list, _ := iss.Pending(ctx); len(list) != 0 {
		t.Fatalf("pairing listed as an agent request: %+v", list)
	}
	if _, err := s.GetPending(ctx, id); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("GetPending: %v", err)
	}
	if _, err := iss.Approve(ctx, id, "someone", nil, "uid:1@local"); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("old Approve: %v", err)
	}
	if err := iss.Deny(ctx, id, "uid:1@local"); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("old Deny: %v", err)
	}
	if _, _, err := s.Collect(ctx, id); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("old Collect: %v", err)
	}
	// And the web client id cannot be parked through /oauth/authorize.
	if _, err := s.CreatePending(ctx, Pending{ClientID: WebClientID}); err == nil {
		t.Fatal("CreatePending accepted the built-in web client")
	}
	if _, err := NewResolver(nil).Resolve(ctx, WebClientID); err == nil {
		t.Fatal("the resolver resolves the built-in web client")
	}
}

func TestPairing_WaitWakesOnApproval(t *testing.T) {
	ctx := context.Background()
	iss, s, _ := newPairingIssuer(t)
	id, code, _ := iss.StartPairing(ctx, "", "")

	// Undecided at the deadline: ErrNotPending (the page polls again).
	if _, err := iss.WaitPairing(ctx, id); !errors.Is(err, ErrNotPending) {
		t.Fatalf("undecided wait: %v, want ErrNotPending", err)
	}
	type res struct {
		cookie string
		err    error
	}
	done := make(chan res, 1)
	iss.waitTimeout = time.Hour
	go func() {
		c, err := iss.WaitPairing(ctx, id)
		done <- res{c, err}
	}()
	waitFor(t, func() bool { return iss.waiterEntries(id) > 0 })
	if err := iss.ApprovePairing(ctx, code, owner(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if _, err := s.LookupSession(ctx, r.cookie); err != nil {
			t.Fatalf("the waited-for cookie is no session: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WaitPairing did not wake on approval")
	}
	if _, err := iss.WaitPairing(ctx, "bogus"); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("bogus id: %v", err)
	}
}

func (i *Issuer) waiterEntries(id string) int {
	i.waiters.mu.Lock()
	defer i.waiters.mu.Unlock()
	if e := i.waiters.m[id]; e != nil {
		return e.refs
	}
	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRefresh_IdleThirtyDaysAbsoluteNinety(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	spec := FamilySpec{ClientID: "kb", Subject: "laptop", Scopes: []string{"read"}, Resource: "https://kb.example"}

	f, err := s.CreateFamily(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := s.IssuePair(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Rotation resets idle: two refreshes 29 days apart both work.
	for range 2 {
		c.add(29 * day)
		if pair, err = s.Refresh(ctx, pair.Refresh, "kb", "", nil); err != nil {
			t.Fatalf("refresh after 29 d: %v", err)
		}
	}
	c.add(31 * day)
	if _, err := s.Refresh(ctx, pair.Refresh, "kb", "", nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("refresh after 31 idle days: %v, want ErrExpired", err)
	}

	// Absolute: refreshed every 20 days, the family still ends at 90.
	f2, _ := s.CreateFamily(ctx, spec)
	pair2, _ := s.IssuePair(ctx, f2.ID)
	for elapsed := 20 * day; elapsed < 90*day; elapsed += 20 * day {
		c.add(20 * day)
		if pair2, err = s.Refresh(ctx, pair2.Refresh, "kb", "", nil); err != nil {
			t.Fatalf("refresh at +%v: %v", elapsed, err)
		}
	}
	c.add(11 * day)
	if _, err := s.Refresh(ctx, pair2.Refresh, "kb", "", nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("refresh at +91 d: %v, want ErrExpired", err)
	}
}
