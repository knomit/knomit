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

// ParsePrincipal must read back what LocalPrincipal writes, on the platform
// it is running on. This is a round-trip through the REAL value rather than a
// literal, because the two halves live in different files and a literal would
// keep passing after either one changed.
//
// It is here because the merge of F19 phase 2 (#252) landed ParsePrincipal
// with a via switch listing ViaCert, ViaToken, ViaSocket and ViaNone — every
// Via that existed on dev, which did not yet have ViaPipe (knomit#245). On
// Windows the local principal is ALWAYS bridge:sid:<SID>@pipe, so every
// `knomit grants add/revoke/list` naming it was rejected with
// `unknown via "pipe"`: the whole grants CLI, unusable for the only local
// principal that platform has.
func TestParsePrincipal_RoundTripsThisPlatformsLocalPrincipal(t *testing.T) {
	me, err := LocalPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	// Positive control on the fixture: a principal that named nobody would
	// round-trip trivially and prove nothing.
	if me.ID == "" || me.Via == "" {
		t.Fatalf("LocalPrincipal produced an empty field: %+v", me)
	}

	got, err := ParsePrincipal(me.String())
	if err != nil {
		t.Fatalf("ParsePrincipal cannot read back this platform's own local principal %q: %v", me.String(), err)
	}
	if got != me {
		t.Fatalf("round trip: %+v, want %+v", got, me)
	}
}

// Both local Vias parse, whichever platform the test runs on. The test above
// only ever exercises one of them, so on its own it would let the other be
// dropped from the switch by the same oversight that dropped ViaPipe.
func TestParsePrincipal_AcceptsBothLocalVias(t *testing.T) {
	for _, s := range []string{
		"bridge:uid:501@socket",
		"bridge:sid:S-1-5-21-1658812953-2110426905-900409081-1002@pipe",
	} {
		p, err := ParsePrincipal(s)
		if err != nil {
			t.Fatalf("ParsePrincipal(%q): %v", s, err)
		}
		if p.String() != s {
			t.Fatalf("ParsePrincipal(%q).String() = %q", s, p.String())
		}
		if p.Kind != KindBridge {
			t.Fatalf("%q: kind = %q, want bridge", s, p.Kind)
		}
	}
	// A SID contains hyphens and digits only, so the id survives the parse
	// whole: kind ends at the FIRST ':' and via starts after the LAST '@',
	// and a SID carries neither character.
	p, err := ParsePrincipal("bridge:sid:S-1-5-21-1-2-3-1001@pipe")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "sid:S-1-5-21-1-2-3-1001" {
		t.Fatalf("id = %q, want the SID intact", p.ID)
	}
}
