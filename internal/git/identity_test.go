package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/git"
)

func TestEnsureKeyPair_GeneratesNewKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")

	signer, fp, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}

	if signer == nil {
		t.Fatal("signer is nil")
	}

	// Check fingerprint length
	if len(fp) != 8 {
		t.Fatalf("fingerprint length = %d, want 8", len(fp))
	}

	// Check private key file exists with correct perms
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("private key perms = %o, want 0600", perm)
	}

	// Check public key file exists with correct perms
	pubPath := keyPath + ".pub"
	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat public key: %v", err)
	}
	if perm := pubInfo.Mode().Perm(); perm != 0644 {
		t.Errorf("public key perms = %o, want 0644", perm)
	}

	// Check public key content starts with ssh-ed25519
	pubData, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	if !strings.HasPrefix(string(pubData), "ssh-ed25519 ") {
		t.Errorf("public key does not start with ssh-ed25519: %s", pubData)
	}
}

func TestEnsureKeyPair_LoadsExistingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")

	_, fp1, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("first EnsureKeyPair: %v", err)
	}

	_, fp2, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("second EnsureKeyPair: %v", err)
	}

	if fp1 != fp2 {
		t.Errorf("fingerprints differ: %s != %s", fp1, fp2)
	}
}

func TestEnsureKeyPair_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "deep", "nested", "dir", "id_ed25519")

	_, _, err := git.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatalf("EnsureKeyPair in nested dir: %v", err)
	}

	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key file not created: %v", err)
	}
}

func TestAgentBranch_Format(t *testing.T) {
	fp := "abcd1234"
	branch := git.AgentBranch(fp)

	if !strings.HasPrefix(branch, "agent/") {
		t.Errorf("branch %q missing agent/ prefix", branch)
	}
	if !strings.HasSuffix(branch, "-"+fp) {
		t.Errorf("branch %q missing -%s suffix", branch, fp)
	}
}

func TestSanitizeHostname(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"laptop", "laptop"},
		{"my host", "my-host"},
		{"host~name", "host-name"},
		{"a:b:c", "a-b-c"},
		{"ok.host.name", "ok.host.name"},
		{"", "local"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := git.SanitizeHostname(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeHostname(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
