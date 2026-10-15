package pki

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func peerURL(addr string) string { return "https://" + addr + "/git/kb" }

func TestClassifyOrigin_AnInstanceOfThisFleetIsAPeer(t *testing.T) {
	x := newTwoInstances(t)
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(x.addr))
	if err != nil || !peer {
		t.Fatalf("peer=%v err=%v, want a peer\nserver log:\n%s", peer, err, x.bLog)
	}
	if n := accepts[x.addr].Load(); n != 1 {
		t.Fatalf("the classification made %d connections, want exactly 1", n)
	}
}

func TestClassifyOrigin_AnotherFleetsInstanceIsNotAPeer(t *testing.T) {
	x := newTwoInstances(t)
	other := newFleet(t)
	m, dir := other.clientSide(t, "outsider")
	peer, err := ClassifyOrigin(ctx10(t), dir, m.keyPath, peerURL(x.addr))
	if err != nil || peer {
		t.Fatalf("peer=%v err=%v, want not a peer and no error", peer, err)
	}
}

// A revoked peer is a peer that FAILS: the verdict is an error, never "not a
// peer", so it can never fall through to stock https.
func TestClassifyOrigin_ARevokedPeerIsAnErrorNotStockHTTPS(t *testing.T) {
	x := newTwoInstances(t)
	x.f.revoke(t, x.b, x.aDir) // A's CRL now lists B
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(x.addr))
	if peer || !errors.Is(err, ErrRevoked) {
		t.Fatalf("peer=%v err=%v, want ErrRevoked", peer, err)
	}
}

// The peer refusing OUR certificate is also an error.
func TestClassifyOrigin_APeerRefusingUsIsAnError(t *testing.T) {
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir) // B's CRL now lists A
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(x.addr))
	if peer || !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("peer=%v err=%v, want ErrRefusedByPeer", peer, err)
	}
}

func TestClassifyOrigin_NoFleetTrustDialsNothing(t *testing.T) {
	key, _ := writeOpenSSHKey(t)
	addr, accepted := notListening(t)
	peer, err := ClassifyOrigin(ctx10(t), filepath.Join(t.TempDir(), "pki"), key, peerURL(addr))
	if accepted() {
		t.Fatal("an instance with no fleet trust dialled the origin")
	}
	if err != nil || peer {
		t.Fatalf("peer=%v err=%v, want not a peer", peer, err)
	}
}

func TestClassifyOrigin_NonHTTPSURLsDialNothing(t *testing.T) {
	x := newTwoInstances(t)
	for _, u := range []string{"http://" + x.addr + "/git/kb", "ssh://git@" + x.addr + "/kb", "git@" + x.addr + ":kb", "/srv/kb"} {
		peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, u)
		if err != nil || peer {
			t.Errorf("%s: peer=%v err=%v, want not a peer", u, peer, err)
		}
	}
	if n := accepts[x.addr].Load(); n != 0 {
		t.Fatalf("%d connections for non-https URLs, want 0", n)
	}
}

// A forge stand-in (a server under a public-style root): not a peer, and no
// request and no client certificate ever reached it.
func TestClassifyOrigin_AForgeIsNotAPeerAndSeesNoRequest(t *testing.T) {
	x := newTwoInstances(t)
	var requests atomic.Int32
	var sawClientCert atomic.Bool
	forge := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			sawClientCert.Store(true)
		}
	}))
	forge.Config.ErrorLog = discardLogger()
	forge.StartTLS()
	t.Cleanup(forge.Close)
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, forge.URL+"/o/kb.git")
	if err != nil || peer {
		t.Fatalf("peer=%v err=%v, want not a peer", peer, err)
	}
	if requests.Load() != 0 || sawClientCert.Load() {
		t.Fatalf("the forge saw %d requests (client cert %v), want none", requests.Load(), sawClientCert.Load())
	}
}

func TestClassifyOrigin_AnUnreachableHostIsRefusedNamed(t *testing.T) {
	x := newTwoInstances(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(addr))
	if peer || !errors.Is(err, ErrOriginUnreachable) {
		t.Fatalf("peer=%v err=%v, want ErrOriginUnreachable", peer, err)
	}
}

func TestClassifyOrigin_TheInternalSchemeIsRefused(t *testing.T) {
	x := newTwoInstances(t)
	for _, u := range []string{"knomit+https://" + x.addr + "/git/kb", "KNOMIT+HTTPS://" + x.addr + "/git/kb"} {
		if _, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, u); !errors.Is(err, ErrUnsupportedOriginScheme) {
			t.Errorf("%s: err=%v, want ErrUnsupportedOriginScheme", u, err)
		}
	}
	if n := accepts[x.addr].Load(); n != 0 {
		t.Fatalf("%d connections, want 0", n)
	}
}

func TestGitURL(t *testing.T) {
	for _, c := range []struct {
		in   string
		peer bool
		want string
		err  bool
	}{
		{"https://h:1/git/kb", true, "knomit+https://h:1/git/kb", false},
		{"HTTPS://h:1/git/kb", true, "knomit+https://h:1/git/kb", false},
		{"http://h:1/git/kb", true, "", true},
		{"git@h:kb", true, "", true},
		{"https://h:1/git/kb", false, "https://h:1/git/kb", false},
		{"git@h:kb", false, "git@h:kb", false},
		{"knomit+https://h:1/git/kb", true, "", true},
		{"knomit+https://h:1/git/kb", false, "", true},
		{"KNOMIT+HTTPS://h:1/git/kb", false, "", true},
	} {
		got, err := GitURL(c.in, c.peer)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("GitURL(%q, %v) = %q, %v; want %q, err %v", c.in, c.peer, got, err, c.want, c.err)
		}
		if c.err && hasScheme(c.in, gitScheme) && !errors.Is(err, ErrUnsupportedOriginScheme) {
			t.Errorf("GitURL(%q): %v is not ErrUnsupportedOriginScheme", c.in, err)
		}
	}
}
