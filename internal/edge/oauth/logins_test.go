package oauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// approveAndCollect drives a request through Decide and Collect, the only
// production path that creates a family, and returns the family's id.
func approveAndCollect(t *testing.T, s *Store, by string) string {
	t.Helper()
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, by); err != nil {
		t.Fatal(err)
	}
	code, _, err := s.Collect(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	cd, err := s.ConsumeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	return cd.Family.ID
}

// Collect carries the request's client_name onto the family (000020) in the
// transaction that creates it, beside who approved it (000011), so the
// operator's list reads "kb (knomit bridge)" and not a client id.
func TestStore_FamilyRecordsTheClientNameAtCollect(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	id := approveAndCollect(t, s, "uid:501@local")
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT client_name FROM oauth_family_clients WHERE family_id = ?`, id).Scan(&name); err != nil {
		t.Fatalf("no side row for the family: %v", err)
	}
	if name != testPending.ClientName {
		t.Fatalf("side row client_name = %q, want %q", name, testPending.ClientName)
	}
	list, err := s.ListLogins(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListLogins = %+v, %v", list, err)
	}
	if l := list[0]; l.ClientName != testPending.ClientName || l.ApprovedBy != "uid:501@local" || l.ClientID != "kb" || l.Subject != "laptop" {
		t.Fatalf("login = %+v", l)
	}
}

// A family with no side rows (minted before 000020, or by a request that
// named no client) lists with an empty name, and the UI shows its client id.
func TestStore_FamilyWithoutSideRowsListsEmptyName(t *testing.T) {
	s, _ := newTestStore(t)
	f, err := s.CreateFamily(context.Background(), testGrant)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListLogins(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != f.ID {
		t.Fatalf("ListLogins = %+v, %v", list, err)
	}
	if list[0].ClientName != "" || list[0].ApprovedBy != "" {
		t.Fatalf("a family without side rows must list empty name and approver: %+v", list[0])
	}
}

// Last used is the newest token's mint time: zero before the first token,
// then the code exchange, then each refresh.
func TestStore_LoginLastUsedIsTheNewestToken(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	f, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	last := func() time.Time {
		t.Helper()
		list, err := s.ListLogins(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("ListLogins = %+v, %v", list, err)
		}
		return list[0].LastUsedAt
	}
	if !last().IsZero() {
		t.Fatal("a family with no token yet must have no last use")
	}
	iss, err := s.IssuePair(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := last(); !got.Equal(c.now()) {
		t.Fatalf("last used = %v, want the first issue %v", got, c.now())
	}
	c.add(3 * time.Hour)
	if _, err := s.Refresh(ctx, iss.Refresh, testGrant.ClientID, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := last(); !got.Equal(c.now()) {
		t.Fatalf("last used = %v, want the refresh %v", got, c.now())
	}
}

// Only usable families are listed: a revoked one and one past its absolute
// refresh end are not. Newest first.
func TestStore_ListLoginsOnlyUsableNewestFirst(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	old, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	mid, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	newest, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeFamily(ctx, mid.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListLogins(ctx)
	if err != nil || len(list) != 2 || list[0].ID != newest.ID || list[1].ID != old.ID {
		t.Fatalf("ListLogins = %+v, %v", list, err)
	}
	// Past old's absolute end (14 days from its creation) but not newest's.
	c.add(14*24*time.Hour - 90*time.Minute)
	if list, err = s.ListLogins(ctx); err != nil || len(list) != 1 || list[0].ID != newest.ID {
		t.Fatalf("a lapsed family must not be listed: %+v, %v", list, err)
	}
}

// RevokeFamily kills every token of the family at once — the access token
// and the refresh token both — and is idempotent. An unknown id is named.
func TestStore_RevokeFamilyKillsItsTokens(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	f, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := s.IssuePair(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateFamily(ctx, testGrant)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.IssuePair(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeFamily(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupAccess(ctx, iss.Access); !errors.Is(err, ErrRevoked) {
		t.Fatalf("access after revoke: %v, want ErrRevoked", err)
	}
	if _, err := s.Refresh(ctx, iss.Refresh, testGrant.ClientID, "", nil); !errors.Is(err, ErrRevoked) {
		t.Fatalf("refresh after revoke: %v, want ErrRevoked", err)
	}
	if _, err := s.LookupAccess(ctx, keep.Access); err != nil {
		t.Fatalf("another family's token must survive: %v", err)
	}
	if err := s.RevokeFamily(ctx, f.ID); err != nil {
		t.Fatalf("revoking again: %v", err)
	}
	if err := s.RevokeFamily(ctx, "no-such-family"); !errors.Is(err, ErrUnknownFamily) {
		t.Fatalf("unknown id: %v, want ErrUnknownFamily", err)
	}
}
