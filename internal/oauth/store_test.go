package oauth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/store/migrate"
)

// openControlDB opens control.db EXACTLY as production does — the same DSN
// and ONE connection (repos.OpenRegistryNoSchema) — and migrates it with the
// real chain. One connection matters: a statement issued on the *sql.DB while
// a transaction holds the only connection deadlocks, and a pooled test DB
// would never show it.
func openControlDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// clock is a settable now() for expiry tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	s := NewStore(openControlDB(t), 2*time.Hour, 14*24*time.Hour)
	s.now = c.now
	return s, c
}

var testGrant = FamilySpec{
	ClientID: "kb",
	Subject:  "laptop",
	Scopes:   []string{"read", "write"},
	Resource: "https://knomit.example.com",
}

func issue(t *testing.T, s *Store) Issued {
	t.Helper()
	fam, err := s.CreateFamily(context.Background(), testGrant)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.IssuePair(context.Background(), fam.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStore_IssueAndLookupByHash(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	out := issue(t, s)

	if out.Access == "" || out.Refresh == "" || out.Access == out.Refresh {
		t.Fatalf("want two distinct tokens, got %+v", out)
	}
	fam, err := s.LookupAccess(ctx, out.Access)
	if err != nil {
		t.Fatalf("LookupAccess: %v", err)
	}
	if fam.Subject != "laptop" || fam.ClientID != "kb" || fam.Resource != testGrant.Resource ||
		strings.Join(fam.Scopes, " ") != "read write" {
		t.Fatalf("family = %+v", fam)
	}

	// No plaintext token anywhere in the table: a copied control.db must
	// yield nothing a client could present.
	rows, err := s.db.Query(`SELECT hash FROM oauth_tokens`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		if h == out.Access || h == out.Refresh || len(h) != 64 {
			t.Fatalf("stored %q: want a 64-hex SHA-256, never the token", h)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("want 2 token rows, got %d", n)
	}
}

func TestStore_UnknownAndWrongKind(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	out := issue(t, s)
	if _, err := s.LookupAccess(ctx, "not-a-token"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("unknown: %v", err)
	}
	// A refresh token is not a bearer credential, and an access token cannot
	// be exchanged.
	if _, err := s.LookupAccess(ctx, out.Refresh); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("refresh presented as access: %v", err)
	}
	if _, err := s.Refresh(ctx, out.Access, "kb", "", nil); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("access presented as refresh: %v", err)
	}
}

func TestStore_ExpiredAccessNotReturned(t *testing.T) {
	s, c := newTestStore(t)
	out := issue(t, s)
	c.add(2*time.Hour - time.Second)
	if _, err := s.LookupAccess(context.Background(), out.Access); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	c.add(time.Second)
	if _, err := s.LookupAccess(context.Background(), out.Access); !errors.Is(err, ErrExpired) {
		t.Fatalf("at expiry: want ErrExpired, got %v", err)
	}
}

func TestStore_RevokeKillsTheFamilyAndIsIdempotent(t *testing.T) {
	for _, which := range []string{"access", "refresh"} {
		t.Run(which, func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			out := issue(t, s)
			tok := out.Access
			if which == "refresh" {
				tok = out.Refresh
			}
			if err := s.Revoke(ctx, tok); err != nil {
				t.Fatal(err)
			}
			if _, err := s.LookupAccess(ctx, out.Access); !errors.Is(err, ErrRevoked) {
				t.Fatalf("access after revoke: %v", err)
			}
			if _, err := s.Refresh(ctx, out.Refresh, "kb", "", nil); !errors.Is(err, ErrRevoked) {
				t.Fatalf("refresh after revoke: %v", err)
			}
			if err := s.Revoke(ctx, tok); err != nil {
				t.Fatalf("second revoke: %v", err)
			}
			if err := s.Revoke(ctx, "never-issued"); err != nil {
				t.Fatalf("revoking an unknown token must succeed (RFC 7009): %v", err)
			}
		})
	}
}

// Rotation, and the reuse rule: a rotated refresh token presented again means
// two parties hold it, and the whole family dies — including the pair the
// legitimate rotation just produced.
func TestStore_RefreshRotatesAndReuseRevokesFamily(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	first := issue(t, s)

	second, err := s.Refresh(ctx, first.Refresh, "kb", "", nil)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.Refresh == first.Refresh || second.Access == first.Access {
		t.Fatal("rotation must mint a NEW pair")
	}
	if second.Family.ID != first.Family.ID {
		t.Fatal("rotation stays in the family")
	}
	if _, err := s.LookupAccess(ctx, second.Access); err != nil {
		t.Fatalf("new access: %v", err)
	}

	if _, err := s.Refresh(ctx, first.Refresh, "kb", "", nil); !errors.Is(err, ErrReused) {
		t.Fatalf("reusing a rotated refresh token: want ErrReused, got %v", err)
	}
	if _, err := s.LookupAccess(ctx, second.Access); !errors.Is(err, ErrRevoked) {
		t.Fatalf("reuse must revoke the family's NEW access token too: %v", err)
	}
	if _, err := s.Refresh(ctx, second.Refresh, "kb", "", nil); !errors.Is(err, ErrRevoked) {
		t.Fatalf("reuse must revoke the family's NEW refresh token too: %v", err)
	}
}

// Refresh lifetime is ABSOLUTE from the family's first issuance: rotating
// never extends it, and the access token minted near the end is capped by it.
func TestStore_RefreshLifetimeIsAbsolute(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	out := issue(t, s)
	end := out.Family.RefreshExpiresAt
	if !end.Equal(c.now().Add(14 * 24 * time.Hour)) {
		t.Fatalf("family end = %v", end)
	}

	c.add(14*24*time.Hour - time.Hour)
	next, err := s.Refresh(ctx, out.Refresh, "kb", "", nil)
	if err != nil {
		t.Fatalf("refresh inside the lifetime: %v", err)
	}
	if !next.Family.RefreshExpiresAt.Equal(end) {
		t.Fatalf("rotation moved the family end from %v to %v", end, next.Family.RefreshExpiresAt)
	}
	if !next.AccessExpiresAt.Equal(end) {
		t.Fatalf("access minted 1h before the family end must be capped at it: %v vs %v", next.AccessExpiresAt, end)
	}

	c.add(time.Hour)
	if _, err := s.Refresh(ctx, next.Refresh, "kb", "", nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("refresh at the family end: want ErrExpired, got %v", err)
	}
}

// Rotation is one transaction: two racing refreshes of the same token cannot
// both win. Exactly one gets a pair; the other is a reuse.
func TestStore_ConcurrentRefreshOneWinner(t *testing.T) {
	s, _ := newTestStore(t)
	out := issue(t, s)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Refresh(context.Background(), out.Refresh, "kb", "", nil)
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrReused), errors.Is(err, ErrRevoked):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly one winner, got %d", wins)
	}
}
