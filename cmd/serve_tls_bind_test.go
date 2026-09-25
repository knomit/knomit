package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/app"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/crashdump"
	"knomit/test/testenv"
)

// knomit#256: `knomit serve` still FAILS THE BOOT when [tls].addr is already
// bound, even though app.OpenTLSServer tells that case apart
// (app.ErrTLSAddrInUse) so the desktop can survive it. "Fails" means what
// #261 made it mean: RunE RETURNS an error wrapping ErrTLSAddrInUse, and the
// process exits through main.go after RunE's defers have run — the crash
// marker's release among them. So this runs serve's whole RunE (the
// embedder swapped for a deterministic one through serveAppOptions) and
// asserts both: the returned error, and no marker left behind.
//
// The certificate is real and wraps serve's own key, so the ONLY thing that
// can fail is the bind; a fixture that failed on the CRL would pass the
// first assertion for the wrong reason, which the ErrTLSAddrInUse check
// rules out.
//
// Sabotage: making serve skip errors.Is(err, app.ErrTLSAddrInUse) (serve on
// without TLS, as the desktop does) turns this red.
func TestServeTLS_BindConflictFailsTheBoot(t *testing.T) {
	home := t.TempDir()
	keyPath, _ := pkitest.NewKey(t)
	for _, suffix := range []string{"", ".pub"} {
		b, err := os.ReadFile(keyPath + suffix)
		if err != nil {
			if suffix == ".pub" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "id_ed25519"+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := pkitest.New(t)
	f.Install(t, f.Enroll(t, "serve", pki.RoleInstance, keyPath), filepath.Join(home, "pki"))

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(plain.Addr().String())
	plain.Close()

	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_HOST", "127.0.0.1")
	t.Setenv("KNOMIT_PORT", port)
	t.Setenv("KNOMIT_TLS_ADDR", held.Addr().String())
	// main.go sets crashdump.Global before any command runs; serve's logger
	// and crash reporter write through it.
	prevGlobal := crashdump.Global
	if crashdump.Global == nil {
		crashdump.Global = crashdump.NewRingWriter(200)
	}
	prevOpts, prevLog, prevLevel := serveAppOptions, log.Logger, zerolog.GlobalLevel()
	serveAppOptions = func(o app.Options) app.Options {
		o.Embedder = &testenv.DeterministicEmbedder{}
		return o
	}
	t.Cleanup(func() {
		serveAppOptions, log.Logger, crashdump.Global = prevOpts, prevLog, prevGlobal
		zerolog.SetGlobalLevel(prevLevel)
	})

	c := serveCmd()
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.SetContext(ctx)
	err = c.RunE(c, nil)
	if err == nil {
		t.Fatal("serve booted with its [tls].addr held")
	}
	if !errors.Is(err, app.ErrTLSAddrInUse) {
		t.Fatalf("serve failed, but not on the TLS bind: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(home, "running.marker")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the crash marker is still there after the returned error (RunE's defers did not run): %v", serr)
	}
}
