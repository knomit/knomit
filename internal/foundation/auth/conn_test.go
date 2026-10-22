package auth

import (
	"context"
	"testing"
)

func TestPeerFromContext_AbsentIsFalse(t *testing.T) {
	if _, ok := PeerFromContext(context.Background()); ok {
		t.Fatal("a bare context carries no peer")
	}
	want := Peer{ID: "uid:501", Via: ViaSocket, PID: 4242}
	got, ok := PeerFromContext(WithPeer(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("WithPeer round trip: got %+v ok=%v, want %+v", got, ok, want)
	}
}

// Peer.Principal is what the middleware calls, and it must carry the peer's
// OWN id and via through rather than re-deriving either: re-deriving is how
// the boot seeding and the middleware came to disagree on Windows
// (knomit#245, defect B).
func TestPeer_PrincipalCarriesIDAndViaThrough(t *testing.T) {
	p := Peer{ID: "sid:S-1-5-21-1-2-3-1001", Via: ViaPipe, PID: 9}.Principal()
	if got := p.String(); got != "bridge:sid:S-1-5-21-1-2-3-1001@pipe" {
		t.Fatalf("principal = %q", got)
	}
	if p.Kind != KindBridge {
		t.Fatalf("kind = %q, want bridge", p.Kind)
	}
}
