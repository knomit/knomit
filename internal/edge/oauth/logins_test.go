package oauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// approveAndCollect drives a request through Decide, Collect and the code
// exchange, the production path that creates a family, and returns its id.
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
	if _, err := s.IssuePair(ctx, cd.Family.ID); err != nil {
		t.Fatal(err)
	}
	return cd.Family.ID
}

// usable creates a family and issues it a pair, so ListLogins lists it.
func usable(t *testing.T, s *Store, spec FamilySpec) Family {
	t.Helper()
	f, err := s.CreateFamily(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssuePair(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	return f
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
	f := usable(t, s, testGrant)
	list, err := s.ListLogins(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != f.ID {
		t.Fatalf("ListLogins = %+v, %v", list, err)
	}
	if list[0].ClientName != "" || list[0].ApprovedBy != "" {
		t.Fatalf("a family without side rows must list empty name and approver: %+v", list[0])
	}
}

// Last used is the newest token's mint time: zero while the code waits to
// be exchanged, then the exchange, then each refresh.
func TestStore_LoginLastUsedIsTheNewestToken(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "uid:501@local"); err != nil {
		t.Fatal(err)
	}
	code, _, err := s.Collect(ctx, p.ID)
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
	cd, err := s.ConsumeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := s.IssuePair(ctx, cd.Family.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := last(); !got.Equal(c.now()) {
		t.Fatalf("last used = %v, want the first issue %v", got, c.now())
	}
	c.add(3 * time.Hour)
	if _, err := s.Refresh(ctx, iss.Refresh, testPending.ClientID, "", nil); err != nil {
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
	old := usable(t, s, testGrant)
	c.add(time.Hour)
	mid := usable(t, s, testGrant)
	c.add(time.Hour)
	newest := usable(t, s, testGrant)
	if _, err := s.RevokeFamily(ctx, mid.ID, "uid:501@local"); err != nil {
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
	rv, err := s.RevokeFamily(ctx, f.ID, "uid:501@local")
	if err != nil {
		t.Fatal(err)
	}
	if rv.RevokedBy != "uid:501@local" || !rv.RevokedAt.Equal(s.now()) {
		t.Fatalf("revocation = %+v, want the revoker and now", rv)
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
	again, err := s.RevokeFamily(ctx, f.ID, "someone-else")
	if err != nil {
		t.Fatalf("revoking again: %v", err)
	}
	if again != rv {
		t.Fatalf("a second revoke must leave the first standing: %+v, want %+v", again, rv)
	}
	if _, err := s.RevokeFamily(ctx, "no-such-family", "uid:501@local"); !errors.Is(err, ErrUnknownFamily) {
		t.Fatalf("unknown id: %v, want ErrUnknownFamily", err)
	}
}

// A family the server revoked itself (a replayed code) has no revoker row,
// and a later operator revoke does not invent one.
func TestStore_ServerRevocationRecordsNoRevoker(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "uid:501@local"); err != nil {
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
	if _, err := s.ConsumeCode(ctx, code); !errors.Is(err, ErrReused) {
		t.Fatalf("replayed code: %v", err)
	}
	rv, err := s.RevokeFamily(ctx, cd.Family.ID, "uid:501@local")
	if err != nil || rv.RevokedBy != "" {
		t.Fatalf("revocation = %+v, %v; want no revoker", rv, err)
	}
}

// A family that can never yield a token is not listed: its code was spent
// by an exchange that then failed, or it lapsed unexchanged. One whose code
// is still live is listed (the client is mid-exchange).
func TestStore_ListLoginsHidesFamiliesThatCanNeverBeUsed(t *testing.T) {
	s, c := newTestStore(t)
	ctx := context.Background()
	collect := func() (string, string) {
		t.Helper()
		p := createPending(t, s)
		if err := s.Decide(ctx, p.ID, DecisionApproved, "laptop", []string{"read"}, "uid:501@local"); err != nil {
			t.Fatal(err)
		}
		code, _, err := s.Collect(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		var fam string
		if err := s.db.QueryRowContext(ctx, `SELECT family FROM oauth_codes WHERE hash = ?`, hashSecret(code)).Scan(&fam); err != nil {
			t.Fatal(err)
		}
		return code, fam
	}
	ids := func() []string {
		t.Helper()
		list, err := s.ListLogins(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range list {
			out = append(out, l.ID)
		}
		return out
	}
	waiting, waitingFam := collect()
	if got := ids(); len(got) != 1 || got[0] != waitingFam {
		t.Fatalf("a live unspent code must be listed: %v", got)
	}
	// Spent by an exchange whose PKCE check then failed: no token, ever.
	if _, err := s.ConsumeCode(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	if got := ids(); len(got) != 0 {
		t.Fatalf("a spent code with no token must not be listed: %v", got)
	}
	// Never exchanged, and its code has expired.
	_, _ = collect()
	c.add(codeTTL)
	if got := ids(); len(got) != 0 {
		t.Fatalf("an expired unexchanged code must not be listed: %v", got)
	}
}
