//go:build !windows

package config_test

import (
	"path/filepath"
	"testing"
)

// A "~/" in the socket value means the user's home directory, on both sides.
// Unix only: on Windows the socket is a pipe name, and a tilde is refused.
func TestSocketPath_AgreesWithLoad_TildeInSocketEnv(t *testing.T) {
	osHome := setOSHome(t)
	isolateSocketEnv(t, t.TempDir())
	t.Setenv("KNOMIT_SOCKET", "~/x.sock")
	requireAgreement(t, filepath.Join(osHome, "x.sock"))
}

func TestSocketPath_AgreesWithLoad_TildeInTOMLSocket(t *testing.T) {
	osHome := setOSHome(t)
	home := t.TempDir()
	isolateSocketEnv(t, home)
	writeSocketTOML(t, home, "~/x.sock")
	requireAgreement(t, filepath.Join(osHome, "x.sock"))
}
