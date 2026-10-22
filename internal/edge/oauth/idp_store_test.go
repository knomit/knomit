package oauth

import (
	"context"
	"errors"
	"testing"
)

// The pending row keeps the HASH of the browser-binding cookie that
// /oauth/authorize set (F19 3c, W1 merged into R1), never the cookie.
func TestStore_PendingKeepsTheBindingHash(t *testing.T) {
	s, _ := newTestStore(t)
	in := testPending
	in.IDPBinding = hashSecret("cookie-value")
	p, err := s.CreatePending(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPending(context.Background(), p.ID)
	if err != nil || got.IDPBinding != hashSecret("cookie-value") {
		t.Fatalf("binding round trip: %q %v", got.IDPBinding, err)
	}
	if list, _ := s.ListPending(context.Background()); len(list) != 1 || list[0].IDPBinding != got.IDPBinding {
		t.Fatalf("ListPending lost the binding: %+v", list)
	}
}

// Every family records who approved it (decided_by of its request), so a
// refresh can tell a path-3 family from the others (ruling W9).
func TestStore_FamilyRecordsWhoApprovedIt(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p := createPending(t, s)
	if err := s.Decide(ctx, p.ID, DecisionApproved, "github-7", []string{"read"}, "idp:github:7"); err != nil {
		t.Fatal(err)
	}
	code, _, err := s.Collect(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	cd, err := s.ConsumeCode(ctx, code)
	if err != nil || cd.Family.ApprovedBy != "idp:github:7" {
		t.Fatalf("family approved_by = %q, %v", cd.Family.ApprovedBy, err)
	}
	iss, err := s.IssuePair(ctx, cd.Family.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := s.LookupAccess(ctx, iss.Access); err != nil || f.ApprovedBy != "idp:github:7" {
		t.Fatalf("LookupAccess family approved_by = %q, %v", f.ApprovedBy, err)
	}
}

// A refresh consults the check BEFORE rotating. A family it refuses is
// revoked outright — its current access token dies with it — and the
// refresh fails with ErrNoLongerAllowed (invalid_grant at the endpoint).
func TestStore_RefreshCheckRevokesARefusedFamily(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	issue := func(by string) Issued {
		f, err := s.CreateFamily(ctx, FamilySpec{ClientID: "kb", Subject: "github-7", Scopes: []string{"read"}, Resource: "https://knomit.example.com", ApprovedBy: by})
		if err != nil {
			t.Fatal(err)
		}
		iss, err := s.IssuePair(ctx, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		return iss
	}
	var asked []string
	s.SetRefreshCheck(func(f Family) bool { asked = append(asked, f.ApprovedBy); return f.ApprovedBy != "idp:github:7" })

	refused := issue("idp:github:7")
	if _, err := s.Refresh(ctx, refused.Refresh, "kb", "", nil); !errors.Is(err, ErrNoLongerAllowed) {
		t.Fatalf("refresh of a refused family: %v", err)
	}
	if _, err := s.LookupAccess(ctx, refused.Access); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the refused family's access token must be dead: %v", err)
	}

	kept := issue("bridge:uid:501@socket")
	if _, err := s.Refresh(ctx, kept.Refresh, "kb", "", nil); err != nil {
		t.Fatalf("refresh of an allowed family: %v", err)
	}
	if len(asked) != 2 {
		t.Fatalf("the check must be asked once per refresh, asked %v", asked)
	}
}
