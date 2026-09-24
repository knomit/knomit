package oauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"knomit/internal/auth"
)

const vIssuer = "https://knomit.example.com"

func mintFor(t *testing.T, s *Store, resource string, scopes ...string) string {
	t.Helper()
	fam, err := s.CreateFamily(context.Background(), FamilySpec{ClientID: "kb", Subject: "laptop", Scopes: scopes, Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.IssuePair(context.Background(), fam.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out.Access
}

func TestVerifier_PrincipalAndCeiling(t *testing.T) {
	s, _ := newTestStore(t)
	v := NewVerifier(vIssuer, s)
	tok := mintFor(t, s, vIssuer, "read", "write")
	p, ceiling, err := v.Verify(context.Background(), tok, "/api/v1/repos")
	if err != nil {
		t.Fatal(err)
	}
	if p != (auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}) || p.String() != "host:laptop@token" {
		t.Fatalf("principal = %+v (%s)", p, p)
	}
	if !ceiling.Has(auth.Read) || !ceiling.Has(auth.Write) || len(ceiling) != 2 {
		t.Fatalf("ceiling = %v", ceiling)
	}
}

// The audience rule (R3): the request's canonical URL — issuer + the path
// the listener sees — must equal the token's resource or sit under it at a
// "/" boundary. A root token covers everything; an MCP-endpoint token covers
// that endpoint only.
func TestVerifier_Audience(t *testing.T) {
	s, _ := newTestStore(t)
	v := NewVerifier(vIssuer, s)
	root := mintFor(t, s, vIssuer, "read")
	work := mintFor(t, s, vIssuer+"/api/v1/repos/work/branches/agent:x/mcp", "read")
	for _, tc := range []struct {
		tok, path string
		ok        bool
	}{
		{root, "/api/v1/repos", true},
		{root, "/api/v1/repos/anything/branches/b/mcp", true},
		{work, "/api/v1/repos/work/branches/agent:x/mcp", true},
		{work, "/api/v1/repos/work/branches/agent:x/mcp/sub", true},
		{work, "/api/v1/repos/workshop/branches/agent:x/mcp", false}, // A vs AB
		{work, "/api/v1/repos/work/branches/agent:x/mcpx", false},
		{work, "/api/v1/repos", false},
		{work, "/api/v1/repos/other/branches/agent:x/mcp", false},
	} {
		_, _, err := v.Verify(context.Background(), tc.tok, tc.path)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.path, err)
		}
		if !tc.ok && !errors.Is(err, ErrWrongAudience) {
			t.Errorf("%s: want ErrWrongAudience, got %v", tc.path, err)
		}
	}
}

// A token minted under one issuer is not accepted after the issuer changes:
// the audience is a URL, and the old one is not under the new issuer.
func TestVerifier_IssuerChangeInvalidates(t *testing.T) {
	s, _ := newTestStore(t)
	tok := mintFor(t, s, vIssuer, "read")
	v := NewVerifier("https://moved.example.com", s)
	if _, _, err := v.Verify(context.Background(), tok, "/api/v1/repos"); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("token for the old issuer: %v", err)
	}
}

func TestVerifier_StoreRefusalsPassThrough(t *testing.T) {
	s, c := newTestStore(t)
	v := NewVerifier(vIssuer, s)
	if _, _, err := v.Verify(context.Background(), "nope", "/"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("unknown: %v", err)
	}
	tok := mintFor(t, s, vIssuer, "read")
	c.add(3 * time.Hour)
	if _, _, err := v.Verify(context.Background(), tok, "/"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	tok = mintFor(t, s, vIssuer, "read")
	_ = s.Revoke(context.Background(), tok)
	if _, _, err := v.Verify(context.Background(), tok, "/"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked: %v", err)
	}
}

// A ceiling naming a permission this build does not know fails closed.
func TestVerifier_UnknownScopeFailsClosed(t *testing.T) {
	s, _ := newTestStore(t)
	v := NewVerifier(vIssuer, s)
	tok := mintFor(t, s, vIssuer, "read", "superpower")
	if _, _, err := v.Verify(context.Background(), tok, "/"); err == nil {
		t.Fatal("a token whose ceiling does not parse was accepted")
	}
}

// The issuer gained a path (https://knomit.example.com → …/knomit): a token
// minted for the bare origin must stop working, although every request URL
// under the new issuer still "extends" the old resource.
func TestVerifier_IssuerGainedAPath(t *testing.T) {
	s, _ := newTestStore(t)
	tok := mintFor(t, s, vIssuer, "read")
	v := NewVerifier(vIssuer+"/knomit", s)
	if _, _, err := v.Verify(context.Background(), tok, "/api/v1/repos"); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("a token for the origin under a path issuer: %v", err)
	}
}
