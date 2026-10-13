//go:build desktop

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// serverExecName is the knomit server binary the bundle carries next to
// knomit-desktop (Contents/MacOS/knomit, usr/bin/knomit in the AppImage,
// knomit.exe beside knomit-desktop.exe on Windows). The Makefile copies the ONE
// `knomit` that `make server` produces; it is never compiled a second time.
const serverExecName = "knomit"

// ServerBinaryMissingError reports that the bundled server binary is not where
// the bundle layout puts it. The desktop never falls back to PATH: a `knomit`
// found there is a different build than the UI it would be paired with.
type ServerBinaryMissingError struct {
	// Path is the location that was expected to hold the binary.
	Path string
	// Err is the underlying stat failure (or a note that Path is a directory).
	Err error
}

func (e *ServerBinaryMissingError) Error() string {
	return fmt.Sprintf("bundled knomit server not found at %s (the app bundle is incomplete): %v", e.Path, e.Err)
}

func (e *ServerBinaryMissingError) Unwrap() error { return e.Err }

// serverBinaryPath resolves the bundled knomit server relative to the running
// desktop executable. It never consults PATH.
func serverBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve desktop executable: %w", err)
	}
	return serverBinaryFor(exe)
}

// serverBinaryFor is serverBinaryPath for a given desktop executable path:
// follow symlinks (the executable may be reached through a link), then take
// the sibling named knomit[.exe] in the real directory. All three bundle
// layouts keep the server beside the desktop binary, so one rule serves
// macOS, the AppImage and Windows.
func serverBinaryFor(exe string) (string, error) {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve desktop executable %s: %w", exe, err)
	}
	want := filepath.Join(filepath.Dir(resolved), serverExecName+exeSuffix)
	fi, err := os.Stat(want)
	if err != nil {
		return "", &ServerBinaryMissingError{Path: want, Err: err}
	}
	if fi.IsDir() {
		return "", &ServerBinaryMissingError{Path: want, Err: errors.New("is a directory")}
	}
	return want, nil
}

// serverVersionTimeout bounds the version probe; a healthy binary prints in
// milliseconds, so a hang means the binary is wedged, not slow.
const serverVersionTimeout = 10 * time.Second

// serverVersion runs `<bin> version` and returns the trimmed first line of its
// output: the full build version (e.g. 0.5.3.6ce780d), the same string
// version.String() gives the desktop. The server's CLI exposes the `version`
// subcommand, not a --version flag.
func serverVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, serverVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s version: %w", bin, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%s version printed nothing", bin)
	}
	return line, nil
}

// checkServerVersion compares the server at bin with the desktop's own version
// string. skew is true when they differ; PR 5's attach-mode warning reads it.
func checkServerVersion(ctx context.Context, bin, desktopVersion string) (serverVer string, skew bool, err error) {
	serverVer, err = serverVersion(ctx, bin)
	if err != nil {
		return "", false, err
	}
	return serverVer, serverVer != desktopVersion, nil
}
