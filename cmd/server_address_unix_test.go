//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// The address `knomit serve` hands its children (recipe `exec`, as
// KNOMIT_SERVER) is its local listener ONLY when that listener bound — at
// cfg.Socket, which here is an operator-named KNOMIT_SOCKET-style path, not
// <home>/knomit.sock — and its TCP address when another process holds the
// socket. Naming a held socket would send this server's children to the
// OTHER instance, which is the bug KNOMIT_SERVER exists to remove.
//
// Sabotage: return ForLocal(cfg.Socket) unconditionally (red: the held case
// names the other instance's socket); derive the path from cfg.Home (red:
// the bound case names a socket nothing listens on).
func TestServerAddress_IsTheListenerThatBound(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ksa") // short: sun_path is ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := config.Defaults()
	cfg.Home = t.TempDir() // NOT where the socket is
	cfg.Socket = filepath.Join(dir, "named.sock")

	t.Run("bound: the socket at cfg.Socket", func(t *testing.T) {
		ul, cleanup, err := openLocalListener(cfg)
		if err != nil || ul == nil {
			t.Fatalf("openLocalListener: %v %v", ul, err)
		}
		defer cleanup()
		if got, want := serverAddress(cfg, ul, "127.0.0.1:19310"), "unix://"+cfg.Socket; got != want {
			t.Fatalf("serverAddress = %q, want %q", got, want)
		}
	})

	t.Run("held by another process: the TCP address", func(t *testing.T) {
		owner, release, err := auth.ListenLocal(cfg.Socket)
		if err != nil || owner == nil {
			t.Fatalf("owner ListenLocal: %v", err)
		}
		defer release()
		ul, cleanup, err := openLocalListener(cfg)
		defer cleanup()
		if err != nil || ul != nil {
			t.Fatalf("a held socket must leave this server TCP-only: ul=%v err=%v", ul, err)
		}
		if got, want := serverAddress(cfg, ul, "0.0.0.0:19310"), "http://127.0.0.1:19310"; got != want {
			t.Fatalf("serverAddress = %q, want %q", got, want)
		}
	})
}
