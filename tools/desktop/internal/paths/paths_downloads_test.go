package paths_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"knomit/tools/desktop/internal/paths"
)

// The Export save dialog opens in the user's Downloads folder.
func TestDownloadsDir_DefaultsToHomeDownloads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows asks the Downloads known folder")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "nocfg"))
	if got, want := paths.DownloadsDir(), filepath.Join(home, "Downloads"); got != want {
		t.Errorf("DownloadsDir = %q, want %q", got, want)
	}
}

func TestDownloadsDir_LinuxHonoursXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG is Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", "/data/dl")
	if got := paths.DownloadsDir(); got != "/data/dl" {
		t.Errorf("env: DownloadsDir = %q, want /data/dl", got)
	}
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	cfg := filepath.Join(home, "cfg")
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "user-dirs.dirs"), []byte("# c\nXDG_DOWNLOAD_DIR=\"$HOME/Telechargements\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := paths.DownloadsDir(), filepath.Join(home, "Telechargements"); got != want {
		t.Errorf("user-dirs.dirs: DownloadsDir = %q, want %q", got, want)
	}
}
