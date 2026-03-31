package identity_test

import (
	"path/filepath"
	"testing"

	"knomit/internal/identity"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestResolveAuth_EmptyConfig(t *testing.T) {
	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != nil {
		t.Fatalf("expected nil auth, got %v", auth)
	}
}

func TestResolveAuth_TokenWithUser(t *testing.T) {
	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "token",
		Token:      "ghp_abc123",
		User:       "myuser",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *BasicAuth, got %T", auth)
	}
	if basic.Username != "myuser" {
		t.Errorf("username = %q, want %q", basic.Username, "myuser")
	}
	if basic.Password != "ghp_abc123" {
		t.Errorf("password = %q, want %q", basic.Password, "ghp_abc123")
	}
}

func TestResolveAuth_TokenWithoutUser(t *testing.T) {
	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "token",
		Token:      "ghp_abc123",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *BasicAuth, got %T", auth)
	}
	if basic.Username != "x-token" {
		t.Errorf("username = %q, want %q", basic.Username, "x-token")
	}
}

func TestResolveAuth_Basic(t *testing.T) {
	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "basic",
		User:       "alice",
		Password:   "secret",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *BasicAuth, got %T", auth)
	}
	if basic.Username != "alice" {
		t.Errorf("username = %q, want %q", basic.Username, "alice")
	}
	if basic.Password != "secret" {
		t.Errorf("password = %q, want %q", basic.Password, "secret")
	}
}

func TestResolveAuth_InferToken(t *testing.T) {
	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		Token: "ghp_inferred",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("expected *BasicAuth, got %T", auth)
	}
	if basic.Username != "x-token" {
		t.Errorf("username = %q, want %q", basic.Username, "x-token")
	}
	if basic.Password != "ghp_inferred" {
		t.Errorf("password = %q, want %q", basic.Password, "ghp_inferred")
	}
}

func TestResolveAuth_UnknownMethod(t *testing.T) {
	_, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "kerberos",
	}, "")
	if err == nil {
		t.Fatal("expected error for unknown method, got nil")
	}
}

func TestResolveAuth_SSHKeyNotFound(t *testing.T) {
	_, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "ssh",
		SSHKey:     "/nonexistent/id_rsa",
	}, "")
	if err == nil {
		t.Fatal("expected error for missing SSH key, got nil")
	}
}

func TestResolveAuth_SSHDefaultKeyFallback(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	_, _, err := identity.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "ssh",
		// SSHKey empty — should fall back to defaultKeyPath.
	}, keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected non-nil auth with default key fallback")
	}
}

func TestResolveAuth_SSHNoKeyAvailable(t *testing.T) {
	// SSH method with no SSHKey and no defaultKeyPath → error.
	_, err := identity.ResolveAuth(identity.RemoteAuthConfig{
		AuthMethod: "ssh",
	}, "")
	if err == nil {
		t.Fatal("expected error when ssh auth has no key path")
	}
}

func TestResolveAuthWithOrigin_GitURL(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	_, _, err := identity.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := identity.ResolveAuthWithOrigin(identity.RemoteAuthConfig{}, keyPath, "git@github.com:org/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected SSH auth for git@ URL")
	}
}

func TestResolveAuthWithOrigin_SSHURL(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	_, _, err := identity.EnsureKeyPair(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := identity.ResolveAuthWithOrigin(identity.RemoteAuthConfig{}, keyPath, "ssh://git@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("expected SSH auth for ssh:// URL")
	}
}

func TestResolveAuthWithOrigin_HTTPSURL(t *testing.T) {
	// HTTPS URL should not auto-detect SSH.
	auth, err := identity.ResolveAuthWithOrigin(identity.RemoteAuthConfig{}, "", "https://github.com/org/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != nil {
		t.Fatal("expected nil auth for HTTPS URL with no credentials")
	}
}
