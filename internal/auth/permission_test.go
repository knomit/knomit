package auth

import (
	"context"
	"errors"
	"testing"
)

func TestParseSet_RejectsUnknownName(t *testing.T) {
	if _, err := ParseSet([]string{"read", "fly"}); err == nil {
		t.Fatal("unknown permission must be an error, not silently dropped")
	}
	s, err := ParseSet([]string{"read", "write"})
	if err != nil || !s.Has(Read) || !s.Has(Write) || s.Has(Admin) {
		t.Fatalf("ParseSet = %v, %v", s, err)
	}
}

func TestAllowed_DefaultDeny(t *testing.T) {
	ctx := context.Background()
	g := StaticGrants{"bridge:uid:501@socket": {Read: {}}}
	p := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}
	if !Allowed(ctx, g, p, Read) {
		t.Fatal("granted read must be allowed")
	}
	if Allowed(ctx, g, p, Write) {
		t.Fatal("ungranted write must be denied")
	}
	other := Principal{Kind: KindBridge, ID: "uid:502", Via: ViaSocket}
	if Allowed(ctx, g, other, Read) {
		t.Fatal("unknown principal must have nothing")
	}
	if Allowed(ctx, g, Principal{}, Read) {
		t.Fatal("zero principal must have nothing")
	}
}

type failingGrants struct{}

func (failingGrants) For(context.Context, Principal) (Set, error) { return nil, errors.New("db down") }

func TestAllowed_StoreErrorDenies(t *testing.T) {
	p := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}
	if Allowed(context.Background(), failingGrants{}, p, Read) {
		t.Fatal("a grants store error must deny, never allow")
	}
}
