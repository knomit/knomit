package git_test

import (
	"testing"

	"knomit/internal/git"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestResolveAuth_EmptyConfig(t *testing.T) {
	auth, err := git.ResolveAuth(git.RemoteAuthConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != nil {
		t.Fatalf("expected nil auth, got %v", auth)
	}
}

func TestResolveAuth_TokenWithUser(t *testing.T) {
	auth, err := git.ResolveAuth(git.RemoteAuthConfig{
		AuthMethod: "token",
		Token:      "ghp_abc123",
		User:       "myuser",
	})
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
	auth, err := git.ResolveAuth(git.RemoteAuthConfig{
		AuthMethod: "token",
		Token:      "ghp_abc123",
	})
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
	auth, err := git.ResolveAuth(git.RemoteAuthConfig{
		AuthMethod: "basic",
		User:       "alice",
		Password:   "secret",
	})
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
	auth, err := git.ResolveAuth(git.RemoteAuthConfig{
		Token: "ghp_inferred",
	})
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
	_, err := git.ResolveAuth(git.RemoteAuthConfig{
		AuthMethod: "kerberos",
	})
	if err == nil {
		t.Fatal("expected error for unknown method, got nil")
	}
}

func TestResolveAuth_SSHKeyNotFound(t *testing.T) {
	_, err := git.ResolveAuth(git.RemoteAuthConfig{
		AuthMethod: "ssh",
		SSHKey:     "/nonexistent/id_rsa",
	})
	if err == nil {
		t.Fatal("expected error for missing SSH key, got nil")
	}
}
