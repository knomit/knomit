package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PublicKeyLine is the line an operator passes to `knomit identity enroll
// --pubkey`, for a desktop user who has no CLI to print it. It must be the
// line ensureKeyPair writes to <key>.pub — the knomit@<host> comment
// included, since enroll takes the SAN host from it — and it is built from
// the PRIVATE key, because .pub can be stale or, with [remote].ssh_key,
// absent.
func TestPublicKeyLine_IsTheLineEnsureKeyPairWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if _, _, err := ensureKeyPair(path); err != nil { // generates, and writes .pub
		t.Fatal(err)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	line, err := PublicKeyLine(path)
	if err != nil {
		t.Fatal(err)
	}
	if line != strings.TrimSuffix(string(pub), "\n") {
		t.Fatalf("PublicKeyLine\n %q\nensureKeyPair's .pub\n %q", line, pub)
	}
	if !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.Contains(line, " knomit@") {
		t.Fatalf("not an authorized_keys line with the knomit@ comment: %q", line)
	}

	// A stale .pub (here: another key's line) does not change the answer.
	if err := os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGarbage knomit@elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, err := PublicKeyLine(path); err != nil || again != line {
		t.Fatalf("PublicKeyLine followed a stale .pub: %q %v", again, err)
	}
}

func TestPublicKeyLine_NoKeyIsAnError(t *testing.T) {
	if _, err := PublicKeyLine(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("PublicKeyLine of a missing key succeeded")
	}
}
