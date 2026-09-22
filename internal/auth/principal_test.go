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

func TestParsePrincipal_IsTheInverseOfString(t *testing.T) {
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, p := range []Principal{
		InstancePrincipal(fp),
		OperatorPrincipal(fp),
		{Kind: KindBridge, ID: "uid:501", Via: ViaSocket},
		{Kind: KindAnonymous, Via: ViaNone},
		{Kind: KindHost, ID: "sub-123", Via: ViaToken},
	} {
		got, err := ParsePrincipal(p.String())
		if err != nil || got != p {
			t.Fatalf("ParsePrincipal(%q) = %+v, %v; want %+v", p.String(), got, err, p)
		}
	}
}

func TestParsePrincipal_RefusesWhatNamesNobody(t *testing.T) {
	for _, s := range []string{
		"", "instance", "instance:abc", // no via
		"agent:abc@cert",    // unknown kind
		"instance:abc@ssh",  // unknown via
		"instance@cert",     // an instance needs an id
		"INSTANCE:abc@cert", // kinds are lowercase constants
		"instance:abc@Cert",
		"anonymous:@none", // empty id spelled with a colon: two spellings of one principal
	} {
		if p, err := ParsePrincipal(s); err == nil {
			t.Fatalf("ParsePrincipal(%q) accepted %+v", s, p)
		}
	}
}
