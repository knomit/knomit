//go:build windows

package config_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// THE PIN between the two halves of the pipe name. config BUILDS the local
// listener path; internal/auth RECOGNISES it, refusing anything outside the
// pipe namespace so a misconfigured path cannot be opened as a file. The
// namespace prefix is therefore spelled in both packages, and this is what
// stops the two spellings drifting: it does not compare the constants, it
// round-trips a real listener through both.
//
// A drift here would not fail loudly on its own — the server would refuse to
// listen and the bridge would fall back to TCP — which is precisely the class
// of silent identity loss the header of paths.go is about.
func TestSocketPath_IsOpenableAndDialableByAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_REPO", "")

	path, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, auth.PipePrefix) {
		t.Fatalf("config built %q, which internal/auth (PipePrefix %q) would refuse to open", path, auth.PipePrefix)
	}

	l, err := auth.ListenLocal(path)
	if err != nil {
		t.Fatalf("auth.ListenLocal could not open what config.SocketPath built (%q): %v", path, err)
	}
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	client, err := auth.DialLocal(context.Background(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("auth.DialLocal could not reach it: %v", err)
	}
	defer client.Close()

	// POSITIVE CONTROL: the dial above would also "succeed" against a pipe
	// nobody was serving if winio were lenient, so assert the LISTENER saw it.
	server := <-accepted
	if server == nil {
		t.Fatal("the listener accepted nothing; this fixture would prove nothing")
	}
	defer server.Close()
	if _, ok := auth.PeerCred(server); !ok {
		t.Fatal("the connection carried no verified peer, so this path gives no identity")
	}
}

// Two data roots must not share a pipe. The namespace is flat and
// machine-wide, so a name that ignored the root would make a second instance
// either fail to listen or, worse, be reached by the first one's bridge.
func TestSocketPath_DiffersPerDataRoot(t *testing.T) {
	first := func(home string) string {
		t.Helper()
		t.Setenv("KNOMIT_HOME", home)
		p, err := config.SocketPath()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := first(t.TempDir())
	b := first(t.TempDir())
	if a == b {
		t.Fatalf("two data roots produced the same pipe %q", a)
	}
}

// Windows paths are case-insensitive, so the same directory spelled two ways
// is ONE data root and must produce ONE pipe. A server started from
// C:\Users\… and a bridge resolving c:\users\… would otherwise miss each
// other and fall silently back to TCP.
func TestSocketPath_IsCaseInsensitiveLikeTheFilesystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	lower, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_HOME", strings.ToUpper(home))
	upper, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if lower != upper {
		t.Fatalf("%q and %q are the same directory but produced %q and %q",
			home, strings.ToUpper(home), lower, upper)
	}

	// POSITIVE CONTROL: the equality above must come from normalisation, not
	// from the name ignoring the data root altogether.
	t.Setenv("KNOMIT_HOME", t.TempDir())
	other, err := config.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if other == lower {
		t.Fatal("the pipe name does not depend on the data root at all")
	}
}
