package auth

import (
	"context"
	"testing"
)

func TestPrincipalString_IsLegibleAndStable(t *testing.T) {
	p := Principal{Kind: KindBridge, ID: "uid:501", Via: ViaSocket}
	if got := p.String(); got != "bridge:uid:501@socket" {
		t.Fatalf("String() = %q", got)
	}
	if got := (Principal{Kind: KindAnonymous, Via: ViaNone}).String(); got != "anonymous@none" {
		t.Fatalf("anonymous String() = %q", got)
	}
}

func TestFromContext_AbsentIsFalse(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context must not carry a principal")
	}
	ctx := WithPrincipal(context.Background(), Principal{Kind: KindBridge, ID: "uid:1", Via: ViaSocket})
	p, ok := FromContext(ctx)
	if !ok || p.ID != "uid:1" {
		t.Fatalf("round trip failed: %+v %v", p, ok)
	}
}
