package authz

import (
	"context"
	"net"
	"testing"
	"time"

	"knomit/internal/foundation/auth"
)

func TestPrincipal_RoundTrip(t *testing.T) {
	cases := []struct {
		p    Principal
		want string
	}{
		{Principal{Kind: KindOS, ID: "uid:1000"}, "os:uid:1000"},
		{Principal{Kind: KindOS, ID: "sid:S-1-5-21-1004336348-1177238915-682003330-512"}, "os:sid:S-1-5-21-1004336348-1177238915-682003330-512"},
		{Principal{Kind: KindToken, ID: "t_7Hk2"}, "token:t_7Hk2"},
		{Principal{Kind: KindNode, ID: "3f9a0c"}, "node:3f9a0c"},
		{Anonymous(), "anonymous"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Fatalf("%+v.String() = %q, want %q", c.p, got, c.want)
		}
		back, err := ParsePrincipal(c.want)
		if err != nil {
			t.Fatalf("ParsePrincipal(%q): %v", c.want, err)
		}
		if back != c.p {
			t.Fatalf("ParsePrincipal(%q) = %+v, want %+v", c.want, back, c.p)
		}

		// The label is a display name, never identity: it is not in the
		// string, and a parsed principal never carries one.
		labelled := c.p
		labelled.Label = "kb on laptop"
		if labelled.String() != c.want {
			t.Fatalf("Label leaked into identity: %q", labelled.String())
		}
	}
}

func TestParsePrincipal_Refuses(t *testing.T) {
	for _, bad := range []string{
		"", "anonymous:x", "os:", "os:1000", "os:uid:", "os:uid:-1", "os:uid:1x",
		"os:sid:", "os:sid:1-5", "token:", "node:", "bridge:uid:1000@socket",
		"oauth:sub", "token:a b", "node:ab\n", "owner", "os:uid:1000@socket",
	} {
		if p, err := ParsePrincipal(bad); err == nil {
			t.Fatalf("ParsePrincipal(%q) accepted as %+v", bad, p)
		}
	}
}

func TestFromPeer_SpellsOS(t *testing.T) {
	got := FromPeer(auth.Peer{ID: "uid:501", Via: auth.ViaSocket, PID: 7})
	if got.String() != "os:uid:501" {
		t.Fatalf("FromPeer = %q, want os:uid:501", got)
	}
	got = FromPeer(auth.Peer{ID: "sid:S-1-5-18", Via: auth.ViaPipe})
	if got.String() != "os:sid:S-1-5-18" {
		t.Fatalf("FromPeer = %q, want os:sid:S-1-5-18", got)
	}
}

// THE one-spelling invariant, carried over from auth's
// TestLocalPrincipal_AgreesWithWhatPeerCredSeesOfUs to the new principal:
// the owner the server compares against and the principal it derives from a
// real local connection must be the same value, or the owner is a stranger
// on their own machine. It connects this process to itself over the
// platform's real local listener (the named pipe on Windows) rather than
// comparing two calls to one function.
func TestOwner_AgreesWithFromPeerOfOurOwnConnection(t *testing.T) {
	path := testLocalListenerPath(t)
	l, cleanup, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatalf("ListenLocal(%q): %v", path, err)
	}
	t.Cleanup(cleanup)

	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()

	client, err := auth.DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("DialLocal(%q): %v", path, err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("accept produced no connection; this fixture would prove nothing")
	}
	defer server.Close()

	// The production path: ConnContext attaches the peer, PeerFromContext
	// reads it back.
	peer, ok := auth.PeerFromContext(auth.ConnContext(context.Background(), server))
	if !ok {
		t.Fatal("no peer credentials on our own local connection")
	}
	owner, err := Owner()
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	// Positive control: two empty principals are equal too.
	if owner.Kind != KindOS || owner.ID == "" {
		t.Fatalf("Owner() = %+v, want a non-empty os principal", owner)
	}
	if got := FromPeer(peer); got != owner {
		t.Fatalf("a request over our own socket is %q, the owner is %q: the owner would be a stranger", got, owner)
	}
	if _, err := ParsePrincipal(owner.String()); err != nil {
		t.Fatalf("the owner's own spelling %q does not parse: %v", owner, err)
	}
}
