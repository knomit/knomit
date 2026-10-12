//go:build desktop

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script that answers `version` with out.
// Skipped on Windows, where a script cannot stand in for an .exe.
func fakeBin(t *testing.T, path, out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary needs a POSIX host")
	}
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo '" + out + "'; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func bundleDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeStub(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerBinaryForSiblingOfExecutable(t *testing.T) {
	dir := bundleDir(t)
	exe := filepath.Join(dir, "knomit-desktop"+exeSuffix)
	srv := filepath.Join(dir, "knomit"+exeSuffix)
	writeStub(t, exe, srv)
	got, err := serverBinaryFor(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != srv {
		t.Errorf("path = %q, want %q", got, srv)
	}
}

func TestServerBinaryForResolvesThroughSymlinkedExecutable(t *testing.T) {
	bundle := bundleDir(t)
	elsewhere := bundleDir(t)
	exe := filepath.Join(bundle, "knomit-desktop"+exeSuffix)
	srv := filepath.Join(bundle, "knomit"+exeSuffix)
	decoy := filepath.Join(elsewhere, "knomit"+exeSuffix)
	writeStub(t, exe, srv, decoy)
	link := filepath.Join(elsewhere, "desktop-link")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("this host cannot create symlinks: %v", err)
	}
	got, err := serverBinaryFor(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != srv {
		t.Errorf("resolved to %q, want the bundle's %q (not the link directory's decoy %q)", got, srv, decoy)
	}
}

func TestServerBinaryForRefusesPATH(t *testing.T) {
	bundle := bundleDir(t)
	pathDir := bundleDir(t)
	// A knomit on PATH, none in the bundle: must NOT be used.
	fakeBin(t, filepath.Join(pathDir, "knomit"), "9.9.9.path")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	exe := filepath.Join(bundle, "knomit-desktop"+exeSuffix)
	writeStub(t, exe)
	got, err := serverBinaryFor(exe)
	if err == nil {
		t.Fatalf("resolved %q from PATH; want a missing-binary error", got)
	}
	var missing *ServerBinaryMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error %T %v, want *ServerBinaryMissingError", err, err)
	}
}

func TestServerBinaryForMissingNamesExpectedPath(t *testing.T) {
	bundle := bundleDir(t)
	exe := filepath.Join(bundle, "knomit-desktop"+exeSuffix)
	writeStub(t, exe)
	_, err := serverBinaryFor(exe)
	want := filepath.Join(bundle, "knomit"+exeSuffix)
	var missing *ServerBinaryMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error %v, want *ServerBinaryMissingError", err)
	}
	if missing.Path != want {
		t.Errorf("Path = %q, want %q", missing.Path, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("message %q does not name %q", err.Error(), want)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error does not unwrap to os.ErrNotExist: %v", err)
	}
}

func TestServerBinaryForDirectoryIsMissing(t *testing.T) {
	bundle := bundleDir(t)
	exe := filepath.Join(bundle, "knomit-desktop"+exeSuffix)
	writeStub(t, exe)
	if err := os.Mkdir(filepath.Join(bundle, "knomit"+exeSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	var missing *ServerBinaryMissingError
	if _, err := serverBinaryFor(exe); !errors.As(err, &missing) {
		t.Fatalf("error %v, want *ServerBinaryMissingError for a directory", err)
	}
}

func TestServerVersionParsesFirstLine(t *testing.T) {
	bin := filepath.Join(bundleDir(t), "knomit")
	fakeBin(t, bin, "0.5.3.abc1234")
	got, err := serverVersion(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.5.3.abc1234" {
		t.Errorf("version = %q, want 0.5.3.abc1234", got)
	}
}

func TestCheckServerVersionSkew(t *testing.T) {
	bin := filepath.Join(bundleDir(t), "knomit")
	fakeBin(t, bin, "0.5.3.abc1234")
	v, skew, err := checkServerVersion(context.Background(), bin, "0.5.3.abc1234")
	if err != nil || skew || v != "0.5.3.abc1234" {
		t.Errorf("same version: got (%q, skew=%v, %v), want (0.5.3.abc1234, false, nil)", v, skew, err)
	}
	v, skew, err = checkServerVersion(context.Background(), bin, "0.5.2.deadbee")
	if err != nil || !skew || v != "0.5.3.abc1234" {
		t.Errorf("different version: got (%q, skew=%v, %v), want (0.5.3.abc1234, true, nil)", v, skew, err)
	}
}

func TestServerVersionErrors(t *testing.T) {
	dir := bundleDir(t)
	if _, err := serverVersion(context.Background(), filepath.Join(dir, "absent")); err == nil {
		t.Error("missing binary: want an error")
	}
	bin := filepath.Join(dir, "silent")
	fakeBin(t, bin, "")
	if _, err := serverVersion(context.Background(), bin); err == nil {
		t.Error("empty output: want an error")
	}
}
