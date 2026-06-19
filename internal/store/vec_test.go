package store

import (
	"os"
	"path/filepath"
	"testing"
)

// When a binary is launched through a stable symlink
// (dist/knomit -> dist/<platform>/knomit), os.Executable() reports the symlink
// path on macOS. Native-lib resolution must follow the symlink so it finds the
// sibling lib/ in the REAL platform directory, not next to the symlink.
func TestDirEvalSymlinksFollowsSymlinkToPlatformDir(t *testing.T) {
	root := t.TempDir()
	platform := filepath.Join(root, "linux-arm64")
	if err := os.MkdirAll(platform, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(platform, "knomit")
	if err := os.WriteFile(realBin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Mirror how the Makefile creates the symlink: relative target.
	link := filepath.Join(root, "knomit")
	if err := os.Symlink(filepath.Join("linux-arm64", "knomit"), link); err != nil {
		t.Fatal(err)
	}

	got := dirEvalSymlinks(link)
	want, err := filepath.EvalSymlinks(platform)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("dirEvalSymlinks(symlink) = %q, want real platform dir %q", got, want)
	}
}

// Without a symlink, resolution returns the plain containing directory.
func TestDirEvalSymlinksPlainPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "knomit")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirEvalSymlinks(bin); got != want {
		t.Fatalf("dirEvalSymlinks(plain) = %q, want %q", got, want)
	}
}
