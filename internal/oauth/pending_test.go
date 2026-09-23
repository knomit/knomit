package oauth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var testPending = Pending{
	ClientID:      "kb",
	ClientName:    "kb (knomit bridge)",
	RedirectURI:   "http://127.0.0.1:5555/callback",
	Scopes:        []string{"read", "write"},
	CodeChallenge: strings.Repeat("A", 43),
	Resource:      "https://knomit.example.com",
	State:         "st",
	RemoteAddr:    "198.51.100.7:40000",
	UserAgent:     "test-agent",
}

func createPending(t *testing.T, s *Store) Pending {
	t.Helper()
	p, err := s.CreatePending(context.Background(), testPending)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStore_PendingRoundTripAndList(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if p.ID == "" || !p.ExpiresAt.Equal(c.now().Add(pendingTTL)) {
		t.Fatalf("created = %+v", p)
	}
	got, err := s.GetPending(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteAddr != testPending.RemoteAddr || got.UserAgent != testPending.UserAgent ||
		got.ClientName != testPending.ClientName || strings.Join(got.Scopes, " ") != "read write" ||
		got.RedirectURI != testPending.RedirectURI || got.Resource != testPending.Resource {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	list, err := s.ListPending(ctx)
	if err != nil || len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("ListPending = %+v, %v", list, err)
	}
	c.add(pendingTTL)
	if list, _ := s.ListPending(ctx); len(list) != 0 {
		t.Fatalf("expired request still listed: %+v", list)
	}
	if _, err := s.GetPending(ctx, "nope"); !errors.Is(err, ErrUnknownPending) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestStore_DecideOnlyOnceAndOnlyLive(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "bridge:uid:501@socket"); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, p.ID, DecisionDenied, "", nil, "x"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second decision: %v", err)
	}
	q := createPending(t, s)
	c.add(pendingTTL)
	if err := s.Decide(ctx, q.ID, DecisionApproved, "laptop", []string{"read"}, "x"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired request approved: %v", err)
	}
}

// Collecting an approval mints the code — once — bound to the client, the
// redirect URI, the challenge and the resource of the request, in a new
// family carrying the approved subject and ceiling.
func TestStore_CollectMintsOneCodeBoundToTheRequest(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if _, _, err := s.Collect(ctx, p.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("collecting an undecided request: %v", err)
	}
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "by"); err != nil {
		t.Fatal(err)
	}
	code, got, err := s.Collect(ctx, p.ID)
	if err != nil || code == "" || got.Decision != DecisionApproved {
		t.Fatalf("Collect = %q, %+v, %v", code, got, err)
	}
	if _, _, err := s.Collect(ctx, p.ID); !errors.Is(err, ErrCollected) {
		t.Fatalf("second collect: %v", err)
	}

	cd, err := s.ConsumeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if cd.ClientID != p.ClientID || cd.RedirectURI != p.RedirectURI || cd.CodeChallenge != p.CodeChallenge ||
		cd.Resource != p.Resource || cd.Family.Subject != "laptop" || strings.Join(cd.Family.Scopes, " ") != "read" ||
		cd.Family.Resource != p.Resource || cd.Family.ClientID != p.ClientID {
		t.Fatalf("code not bound to the request: %+v", cd)
	}
}

func TestStore_CollectDenied(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionDenied, "", nil, "by"); err != nil {
		t.Fatal(err)
	}
	code, got, err := s.Collect(ctx, p.ID)
	if err != nil || code != "" || got.Decision != DecisionDenied {
		t.Fatalf("denied collect = %q, %+v, %v", code, got, err)
	}
}

// A code is single-use; presenting it again revokes the family the first
// exchange started, tokens included.
func TestStore_CodeReuseRevokesFamily(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	_ = s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "by")
	code, _, _ := s.Collect(ctx, p.ID)
	cd, err := s.ConsumeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := s.IssuePair(ctx, cd.Family.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeCode(ctx, code); !errors.Is(err, ErrReused) {
		t.Fatalf("replayed code: %v", err)
	}
	if _, err := s.LookupAccess(ctx, pair.Access); !errors.Is(err, ErrRevoked) {
		t.Fatalf("code replay must revoke the tokens it produced: %v", err)
	}
}

func TestStore_CodeExpiresAndDiesWithFamily(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()

	p := createPending(t, s)
	_ = s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "by")
	code, _, _ := s.Collect(ctx, p.ID)
	c.add(codeTTL)
	if _, err := s.ConsumeCode(ctx, code); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired code: %v", err)
	}

	q := createPending(t, s)
	_ = s.Decide(ctx, q.ID, DecisionApproved, "laptop", []string{"read"}, "by")
	code, _, _ = s.Collect(ctx, q.ID)
	var fam string
	if err := s.db.QueryRow(`SELECT family FROM oauth_codes WHERE hash = ?`, hashSecret(code)).Scan(&fam); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE oauth_families SET revoked_at = 1 WHERE id = ?`, fam); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeCode(ctx, code); !errors.Is(err, ErrRevoked) {
		t.Fatalf("unconsumed code of a revoked family: %v", err)
	}
}

// /oauth/authorize is unauthenticated, so how many requests can wait at once
// is bounded; expired ones stop counting.
func TestStore_LivePendingIsCapped(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < maxLivePending; i++ {
		createPending(t, s)
	}
	if _, err := s.CreatePending(ctx, testPending); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("over the cap: %v", err)
	}
	c.add(pendingTTL)
	if _, err := s.CreatePending(ctx, testPending); err != nil {
		t.Fatalf("expired requests must not count: %v", err)
	}
}

// Refresh checks the client and the audience INSIDE its transaction, before
// rotating: a refused refresh leaves the token usable by its real owner.
func TestStore_RefreshChecksClientAndResourceBeforeRotating(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	out := issue(t, s)
	if _, err := s.Refresh(ctx, out.Refresh, "someone-else", ""); !errors.Is(err, ErrWrongClient) {
		t.Fatalf("other client: %v", err)
	}
	if _, err := s.Refresh(ctx, out.Refresh, "kb", "https://knomit.example.com/api/v1/other"); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("other resource: %v", err)
	}
	if _, err := s.Refresh(ctx, out.Refresh, "kb", ""); err != nil {
		t.Fatalf("the owner, after two refused attempts: %v", err)
	}
}

// An approval the browser collects only after the request expired is not
// honoured: whoever was waiting has gone.
func TestStore_CollectAfterExpiryRefused(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "by"); err != nil {
		t.Fatal(err)
	}
	c.add(pendingTTL)
	if code, _, err := s.Collect(ctx, p.ID); !errors.Is(err, ErrExpired) || code != "" {
		t.Fatalf("late collect = %q, %v", code, err)
	}
}
