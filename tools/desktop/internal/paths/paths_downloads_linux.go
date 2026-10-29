//go:build linux

package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// downloadsDir: $XDG_DOWNLOAD_DIR, else XDG_DOWNLOAD_DIR in ~/.config/user-dirs.dirs
// (honouring $XDG_CONFIG_HOME), else ~/Downloads.
func downloadsDir() string {
	if d := os.Getenv("XDG_DOWNLOAD_DIR"); filepath.IsAbs(d) {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	if b, err := os.ReadFile(filepath.Join(cfg, "user-dirs.dirs")); err == nil {
		if d := parseUserDirs(string(b), home); d != "" {
			return d
		}
	}
	return filepath.Join(home, "Downloads")
}

// parseUserDirs reads the XDG_DOWNLOAD_DIR line of a user-dirs.dirs file:
// XDG_DOWNLOAD_DIR="$HOME/Downloads". "" when absent or not absolute.
func parseUserDirs(contents, home string) string {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "XDG_DOWNLOAD_DIR=")
		if !ok {
			continue
		}
		v := strings.Trim(strings.TrimSpace(rest), `"'`)
		v = strings.ReplaceAll(v, "$HOME", home)
		if filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
	}
	return ""
}
