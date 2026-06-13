//go:build darwin

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinAutostart_EnableDisable(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "knomit-desktop")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &darwin{
		binaryPath: fakeBin,
		plistPath:  filepath.Join(dir, "com.knomit.desktop.plist"),
		loader:     fakeLauncher{},
	}

	if enabled, err := a.Enabled(); err != nil || enabled {
		t.Fatalf("Enabled initially = (%v, %v), want (false, nil)", enabled, err)
	}
	if err := a.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	data, err := os.ReadFile(a.plistPath)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if !strings.Contains(string(data), fakeBin) {
		t.Errorf("plist missing binary path; got:\n%s", data)
	}
	if enabled, err := a.Enabled(); err != nil || !enabled {
		t.Errorf("Enabled after Enable = (%v, %v), want (true, nil)", enabled, err)
	}

	if err := a.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(a.plistPath); !os.IsNotExist(err) {
		t.Errorf("plist not removed: %v", err)
	}
}

type fakeLauncher struct{}

func (fakeLauncher) load(string) error   { return nil }
func (fakeLauncher) unload(string) error { return nil }
