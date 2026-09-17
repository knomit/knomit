package sessions

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMintBindingHandle_SetThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, ok, err := s.BindingHandle(ctx, "nobody-minted-this", t0); err != nil || ok {
		t.Fatalf("unknown handle: ok=%v err=%v", ok, err)
	}
	if err := s.MintBindingHandle(ctx, "h1", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	pin, branch, ok, err := s.BindingHandle(ctx, "h1", t0)
	if err != nil || !ok || pin != "repo:u1" {
		t.Fatalf("pin=%q ok=%v err=%v", pin, ok, err)
	}
	if branch != "" {
		t.Fatalf(`branch=%q; "" means the target's own read branch`, branch)
	}
}

// THE INCIDENT, at the storage layer. Two handles coexist and neither
// overwrites the other — which is the entire difference from the session-keyed
// table this replaced, where a second bind on one session id silently
// redirected the first caller.
func TestMintBindingHandle_HandlesCoexist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.MintBindingHandle(ctx, "hA", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MintBindingHandle(ctx, "hB", "lens:l1", "", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if pin, _, _, _ := s.BindingHandle(ctx, "hA", t0); pin != "repo:u1" {
		t.Fatalf("the second bind changed the first handle's target: pin=%q", pin)
	}
	if pin, _, _, _ := s.BindingHandle(ctx, "hB", t0); pin != "lens:l1" {
		t.Fatalf("pin=%q", pin)
	}
}

// Re-minting an existing handle is refused rather than silently retargeting it.
// Unreachable through knomit_bind — every call mints fresh random bytes — but
// the INSERT is what makes "a handle names what it named when minted" a
// property of the table and not merely of its caller.
func TestMintBindingHandle_NoSilentRetarget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.MintBindingHandle(ctx, "h1", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MintBindingHandle(ctx, "h1", "repo:u2", "", t0); err == nil {
		t.Fatal("re-minting an existing handle was accepted")
	}
	if pin, _, _, _ := s.BindingHandle(ctx, "h1", t0); pin != "repo:u1" {
		t.Fatalf("target changed: pin=%q", pin)
	}
}

func TestMintBindingHandle_EmptyArgsRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.MintBindingHandle(context.Background(), "", "repo:u1", "", t0); err == nil {
		t.Fatal("empty handle accepted")
	}
	if err := s.MintBindingHandle(context.Background(), "h", "", "", t0); err == nil {
		t.Fatal("empty pin accepted")
	}
}

// A handle carries at least 128 bits of entropy and no structure a model could
// reconstruct from a name it has seen.
func TestNewBindingHandle_OpaqueAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		h, err := NewBindingHandle()
		if err != nil {
			t.Fatal(err)
		}
		if seen[h] {
			t.Fatalf("duplicate handle %q", h)
		}
		seen[h] = true
		// base64url of 24 bytes, unpadded: 32 chars, 192 bits.
		if len(h) != 32 {
			t.Fatalf("handle %q is %d chars; want 32 (192 bits)", h, len(h))
		}
		if strings.ContainsAny(h, "+/=") {
			t.Fatalf("handle %q is not URL-safe unpadded base64", h)
		}
	}
}

// Handles age on their OWN last_used_at, not on any session's life. A handle
// still in use survives a purge that removes an abandoned one — and the used
// one is kept alive by the READ, which is what stamps last_used_at.
func TestPurge_AgesOutUnusedHandles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.MintBindingHandle(ctx, "stale", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MintBindingHandle(ctx, "busy", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	// "busy" is used just before the purge horizon; "stale" has not been
	// touched since t0.
	if _, _, ok, _ := s.BindingHandle(ctx, "busy", t0.Add(199*time.Hour)); !ok {
		t.Fatal("busy handle missing before purge")
	}
	if _, err := s.Purge(ctx, t0.Add(200*time.Hour)); err != nil { // retention 168h
		t.Fatal(err)
	}
	if _, _, ok, _ := s.BindingHandle(ctx, "stale", t0); ok {
		t.Fatal("a handle unused for the whole retention window survived purge")
	}
	if _, _, ok, _ := s.BindingHandle(ctx, "busy", t0); !ok {
		t.Fatal("a handle in active use was purged")
	}
}

// A handle outlives the client_sessions rows around it: it is not keyed by
// session id, so no session's death may collect it.
func TestPurge_HandleSurvivesItsSessionsDeath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Touch(ctx, bridgeObs("old-session", t0))
	if err := s.MintBindingHandle(ctx, "h", "repo:u1", "", t0); err != nil {
		t.Fatal(err)
	}
	// The session ages out; the handle is used right up to the horizon.
	if _, _, ok, _ := s.BindingHandle(ctx, "h", t0.Add(199*time.Hour)); !ok {
		t.Fatal("handle missing")
	}
	if _, err := s.Purge(ctx, t0.Add(200*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.BindingHandle(ctx, "h", t0); !ok {
		t.Fatal("a live handle died with the session row it was minted under")
	}
}
