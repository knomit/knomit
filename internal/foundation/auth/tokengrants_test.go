package auth

import (
	"context"
	"errors"
	"testing"
)

type errGrants struct{}

func (errGrants) For(context.Context, Principal) (Set, error) { return nil, errors.New("down") }

var tokenP = Principal{Kind: KindHost, ID: "laptop", Via: ViaToken}

// A token principal holds grants(principal) ∩ ceiling: the rows say what the
// SUBJECT may do, the ceiling what THIS TOKEN was approved for.
func TestTokenGrants_Intersection(t *testing.T) {
	g := TokenGrants{Inner: StaticGrants{tokenP.String(): {Read: {}, Write: {}}}}
	ctx := WithCeiling(context.Background(), Set{Read: {}, Operator: {}})
	set, err := g.For(ctx, tokenP)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Has(Read) || set.Has(Write) || set.Has(Operator) || len(set) != 1 {
		t.Fatalf("want exactly {read}: rows {read, write} ∩ ceiling {read, operator}; got %v", set)
	}
}

// No ceiling on the context means the token was not verified here: nothing.
func TestTokenGrants_MissingCeilingIsEmpty(t *testing.T) {
	g := TokenGrants{Inner: StaticGrants{tokenP.String(): {Read: {}, Write: {}}}}
	set, err := g.For(context.Background(), tokenP)
	if err != nil || len(set) != 0 {
		t.Fatalf("missing ceiling: %v, %v; want empty", set, err)
	}
	if Allowed(context.Background(), g, tokenP, Read) {
		t.Fatal("Allowed(read) with no ceiling")
	}
}

// Every other principal passes straight through, ceiling or not: a ceiling
// on the context never narrows a socket or certificate principal.
func TestTokenGrants_OtherPrincipalsUntouched(t *testing.T) {
	sock := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}
	g := TokenGrants{Inner: StaticGrants{sock.String(): {Read: {}, Write: {}}}}
	ctx := WithCeiling(context.Background(), Set{Read: {}})
	set, err := g.For(ctx, sock)
	if err != nil || !set.Has(Write) {
		t.Fatalf("socket principal narrowed by a ceiling: %v, %v", set, err)
	}
}

func TestTokenGrants_StoreErrorPropagates(t *testing.T) {
	g := TokenGrants{Inner: errGrants{}}
	ctx := WithCeiling(context.Background(), Set{Read: {}})
	if _, err := g.For(ctx, tokenP); err == nil {
		t.Fatal("a store error must reach Allowed, which denies on it")
	}
}
