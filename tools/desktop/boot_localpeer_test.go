//go:build desktop

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"knomit/internal/auth"
)

// Helpers shared by the two halves of the desktop's local-listener tests:
// boot_socket_unix_test.go (a unix socket) and boot_pipe_windows_test.go (a
// named pipe). Nothing here knows which transport it is on.

// peerEcho answers with what ConnContext attached to the request, so a test
// can tell a listener that merely serves from one that carries peer
// credentials. The ID is echoed VERBATIM and not reformatted: since knomit#245
// it is "uid:<n>" on unix and "sid:<SID>" on Windows, and a test that spelled
// it itself would be asserting its own arithmetic.
var peerEcho = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	peer, ok := auth.PeerFromContext(r.Context())
	fmt.Fprintf(w, "peer id=%q ok=%v", peer.ID, ok)
})

// wantOwnPeer is what peerEcho prints for a connection from this process over
// the local listener. auth.LocalPrincipal is the one place that spelling comes
// from on either platform, which is the invariant knomit#245 exists for.
func wantOwnPeer(t *testing.T) string {
	t.Helper()
	me, err := auth.LocalPrincipal()
	if err != nil {
		t.Fatal(err)
	}
	if me.ID == "" {
		t.Fatal("LocalPrincipal produced an empty ID; this fixture would assert nothing")
	}
	return fmt.Sprintf("peer id=%q ok=true", me.ID)
}

// wantNoPeer is what peerEcho prints for a connection with no credential.
const wantNoPeer = `peer id="" ok=false`

// localClient reaches the local listener at path whatever it is, through the
// same auth.DialLocal the real bridge uses — including, on Windows, the
// impersonation level without which the server can read no SID.
func localClient(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return auth.DialLocal(ctx, path, 5*time.Second)
		}},
	}
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
