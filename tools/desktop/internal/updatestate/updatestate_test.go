package updatestate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")

	if err := Save(path, State{LastNotified: "0.5.1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := Load(path).LastNotified; got != "0.5.1" {
		t.Errorf("LastNotified = %q, want 0.5.1", got)
	}

	// A later version replaces it rather than accumulating.
	if err := Save(path, State{LastNotified: "0.5.2"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if got := Load(path).LastNotified; got != "0.5.2" {
		t.Errorf("LastNotified = %q, want 0.5.2", got)
	}
}

// Save creates the state directory. On a fresh install nothing has written to
// it yet, and the updater must not be the one call that needs it to exist
// already.
func TestSaveCreatesTheStateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "update.json")

	if err := Save(path, State{LastNotified: "0.5.1"}); err != nil {
		t.Fatalf("Save into a missing directory: %v", err)
	}
	if got := Load(path).LastNotified; got != "0.5.1" {
		t.Errorf("LastNotified = %q, want 0.5.1", got)
	}
}

// Load never fails. The only consequence of losing this memory is one
// redundant banner, so every unreadable case must degrade to "nothing has been
// notified" rather than give a caller a reason to suppress an update.
func TestLoadDegradesToZeroStateOnAnyProblem(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.json")
	if got := Load(missing).LastNotified; got != "" {
		t.Errorf("Load(missing) = %q, want empty", got)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load(corrupt).LastNotified; got != "" {
		t.Errorf("Load(corrupt) = %q, want empty", got)
	}

	// A TRUNCATED file — what an interrupted write would leave if Save were
	// not atomic. json.Unmarshal validates the whole document before
	// assigning anything, so the destination is untouched here rather than
	// half-populated; this pins that behaviour rather than the explicit
	// zero-value return, which is belt-and-braces for a future switch to a
	// streaming Decoder.
	truncated := filepath.Join(dir, "truncated.json")
	if err := os.WriteFile(truncated, []byte(`{"last_notified":"0.5.1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load(truncated).LastNotified; got != "" {
		t.Errorf("Load(truncated) = %q, want empty — a half-parsed file must not be trusted", got)
	}

	// A directory where a file should be — os.ReadFile errors rather than
	// returning bytes.
	asDir := filepath.Join(dir, "update.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Load(asDir).LastNotified; got != "" {
		t.Errorf("Load(directory) = %q, want empty", got)
	}
}

// The file records who the user is and what they run; 0600 keeps it to them,
// matching the server lockfile written into the same directory.
func TestSaveWritesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	if err := Save(path, State{LastNotified: "0.5.1"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
}

// Write-then-rename: a failed or interrupted save must not leave a truncated
// file where a valid one was, and must leave no debris behind.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update.json")
	for _, v := range []string{"0.5.1", "0.5.2", "0.5.3"} {
		if err := Save(path, State{LastNotified: v}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "update.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir holds %v, want just update.json", names)
	}
}

// An unknown field must not wipe the state. Files written by a newer build
// have to survive a downgrade, or rolling back would silently re-banner.
func TestLoadIgnoresUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(path, []byte(`{"last_notified":"0.5.1","skipped":"0.4.9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Load(path).LastNotified; got != "0.5.1" {
		t.Errorf("LastNotified = %q, want 0.5.1", got)
	}
}
