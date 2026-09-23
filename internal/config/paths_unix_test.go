//go:build !windows

package config_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// macOS and Linux keep ~/.knomit. The Windows default moved because Windows had
// not shipped; these have an installed base, and moving them is a separate
// decision with a migration attached.
//
// This pins both halves config owns: the dotted NAME, and the POLICY that it
// hangs off the home directory rather than off the OS state directory as on
// Windows.
func TestDefaultHome_IsDotKnomitInHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := config.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(dir, ".knomit"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
}

// Same contract as the Windows twin: the OS refusal is userdirs', naming
// KNOMIT_HOME is config's, and an unresolvable home is an error AND an empty
// string — never a path built from "" that lands at the filesystem root.
func TestDefaultHome_ErrorsRatherThanReturningTheFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := config.DefaultHome()
	if err == nil {
		// os.UserHomeDir on unix consults only $HOME, but a platform that
		// resolves it another way is not a failure of this contract.
		if got == filepath.Join("", ".knomit") || got == "/.knomit" {
			t.Fatalf("DefaultHome = %q — a root-relative path built from an empty home", got)
		}
		t.Skipf("this platform resolved a home (%q) without $HOME", got)
	}
	if got != "" {
		t.Errorf("DefaultHome returned %q alongside its error; want \"\"", got)
	}
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not mention KNOMIT_HOME, the documented way out", err)
	}
}

func TestLockfilePath_IsServerJSONInStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state, err := config.StateDir()
	if err != nil {
		t.Skipf("no state dir on %s: %v", os.Getenv("GOOS"), err)
	}
	got, err := config.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if want := filepath.Join(state, "server.json"); got != want {
		t.Errorf("LockfilePath = %q, want %q", got, want)
	}
}

// --- knomit#253: a data root too long for sun_path ---

// longHome is a 120-byte data root: <home>/knomit.sock exceeds the sun_path
// cap on every unix (104 darwin, 108 linux). It is never created; nothing
// here needs it to exist.
func longHome(t *testing.T) string {
	t.Helper()
	h := "/tmp/" + strings.Repeat("h", 115)
	if len(h) != 120 {
		t.Fatalf("longHome is %d bytes", len(h))
	}
	return h
}

func wantFallback(home string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	return filepath.Join("/tmp", "knomit-"+strconv.Itoa(os.Geteuid()), hex.EncodeToString(sum[:])[:8]+".sock")
}

func socketPathFor(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("KNOMIT_HOME", home)
	t.Setenv("KNOMIT_SOCKET", "")
	p, err := config.SocketPath()
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	return p
}

// A home that fits is byte-for-byte what it was before #253.
func TestSocketPath_ShortHomeIsUnchanged(t *testing.T) {
	home := "/tmp/kh"
	if got, want := socketPathFor(t, home), "/tmp/kh/knomit.sock"; got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
}

func TestSocketPath_LongHomeFallsBackUnderTmpPerUser(t *testing.T) {
	home := longHome(t)
	got := socketPathFor(t, home)
	if got != wantFallback(home) {
		t.Fatalf("SocketPath() = %q, want %q", got, wantFallback(home))
	}
	if len(got) >= auth.SunPathCap() {
		t.Fatalf("the fallback %q is %d bytes, not under the cap %d", got, len(got), auth.SunPathCap())
	}
	if filepath.Dir(got) != auth.FallbackSocketDir() {
		t.Fatalf("the fallback %q is not in auth.FallbackSocketDir() %q", got, auth.FallbackSocketDir())
	}
}

// E3: the path is a function of the data root and the euid ONLY. A bridge
// started with a filtered or different environment must compute the same
// path as the server, so TMPDIR and XDG_RUNTIME_DIR must not move it.
func TestSocketPath_FallbackIgnoresTmpdirAndXDGRuntimeDir(t *testing.T) {
	home := longHome(t)
	clean := socketPathFor(t, home)
	t.Setenv("TMPDIR", "/nonexistent/garbage/tmpdir")
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/garbage/xdg")
	if got := socketPathFor(t, home); got != clean {
		t.Fatalf("with TMPDIR/XDG_RUNTIME_DIR set, SocketPath() = %q, want %q", got, clean)
	}
}

// The hash is of the CLEANED root, so spellings of one directory agree.
func TestSocketPath_FallbackHashesTheCleanedHome(t *testing.T) {
	home := longHome(t)
	want := socketPathFor(t, home)
	for _, spelling := range []string{home + "/", home + "/.", "/tmp//" + strings.TrimPrefix(home, "/tmp/")} {
		if got := socketPathFor(t, spelling); got != want {
			t.Errorf("home %q gives %q, want %q", spelling, got, want)
		}
	}
	// Positive control: a DIFFERENT long home gets a different socket.
	if other := socketPathFor(t, home+"x"); other == want {
		t.Fatal("two different data roots share a fallback socket")
	}
}

// D9: the SERVER's path (Load().Socket, which `knomit serve` and the desktop
// open) and the BRIDGE's (config.SocketPath, which knomitapi dials) must be
// the same string. They are computed on two different routes; this is the
// pair that can drift.
func TestSocketPath_ServerAndBridgeAgreeForALongHome(t *testing.T) {
	home := longHome(t)
	bridge := socketPathFor(t, home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Socket != bridge {
		t.Fatalf("server Load().Socket = %q, bridge SocketPath() = %q", cfg.Socket, bridge)
	}
}

// The computed fallback is not just a string: auth.ListenLocal opens it and
// auth.DialLocal reaches it. This uses the REAL /tmp/knomit-<euid>, which is
// where production puts it; the socket name is this test's own hash, so it
// cannot collide with a running server's.
func TestSocketPath_FallbackIsOpenableAndDialableByAuth(t *testing.T) {
	home := longHome(t) + "-listen-test"
	p := socketPathFor(t, home)
	ln, cleanup, err := auth.ListenLocal(p)
	if err != nil {
		t.Fatalf("auth.ListenLocal(%q): %v", p, err)
	}
	t.Cleanup(func() { cleanup(); os.Remove(p + ".lock") })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	c, err := auth.DialLocal(t.Context(), p, time.Second)
	if err != nil {
		t.Fatalf("auth.DialLocal(%q): %v", p, err)
	}
	c.Close()
}
