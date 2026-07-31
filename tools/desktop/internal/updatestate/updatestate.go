// Package updatestate persists the desktop updater's cross-launch memory:
// which version the user has already been shown an update banner for.
//
// It exists because the banner is a one-time EVENT while the tray badge is
// persistent STATE. Without this file that distinction only held within a
// single run — every relaunch re-announced a version the user had already been
// told about, so "once per version" was really "once per version per launch".
//
// Deliberately NOT part of internal/config, which is load-at-startup TOML/env
// with no writer. This is runtime state, and it lives beside the server
// lockfile in paths.StateDir().
package updatestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is the whole file. One field today; a struct rather than a bare string
// so adding another (a skipped version, a last-check timestamp) does not
// invalidate files already on disk.
type State struct {
	// LastNotified is the version the user was last shown a banner for.
	// Empty means no banner has been posted on this machine.
	LastNotified string `json:"last_notified"`
}

// Load reads the state at path.
//
// It NEVER fails. A missing file, an unreadable one and a corrupt one all
// return the zero State, because the only consequence of losing this memory is
// one redundant banner. Propagating an error here would invite a caller to
// treat an unreadable JSON file as a reason not to announce an update — which
// is strictly worse than the annoyance the file exists to prevent.
func Load(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}
	}
	return s
}

// Save writes s to path atomically (write-then-rename) with 0600 perms,
// creating parent directories as needed.
//
// The error IS returned, unlike Load's: a failed save means the next launch
// re-banners, and that is worth a line in the log even though it is not worth
// interrupting anything over.
func Save(path string, s State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal update state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "update-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename tempfile: %w", err)
	}
	return nil
}
