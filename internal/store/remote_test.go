package store_test

import (
	"testing"

	"knomit/internal/store"
)

func TestSetRemoteWithAuth_NoCrypt(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if err := svc.SetRemoteWithAuth("origin", "https://example.com/kb.git", "main", 300, 300, "token", "secret"); err != nil {
		t.Fatal(err)
	}

	r, err := svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected non-nil remote")
	}
	if r.AuthMethod != "token" {
		t.Errorf("auth_method = %q, want %q", r.AuthMethod, "token")
	}
	// Without crypt, token stored and returned as-is.
	if r.AuthToken != "secret" {
		t.Errorf("auth_token = %q, want %q", r.AuthToken, "secret")
	}
}

func TestSetRemoteWithAuth_WithCrypt(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	c, err := store.NewCrypt([]byte("test-key-material-32-bytes-padded"))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetCrypt(c)

	if err := svc.SetRemoteWithAuth("origin", "https://example.com/kb.git", "main", 300, 300, "token", "mysecret"); err != nil {
		t.Fatal(err)
	}

	r, err := svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected non-nil remote")
	}
	// GetRemote decrypts, so we should see the original plaintext.
	if r.AuthToken != "mysecret" {
		t.Errorf("auth_token = %q, want %q", r.AuthToken, "mysecret")
	}
}

func TestRemoteCRUD(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// GetRemote returns nil for non-existent remote.
	r, err := svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r != nil {
		t.Fatal("expected nil for non-existent remote")
	}

	// SetRemote creates a remote.
	if err := svc.SetRemote("origin", "https://example.com/kb.git", "main", 300, 300); err != nil {
		t.Fatal(err)
	}

	r, err = svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected non-nil remote after SetRemote")
	}
	if r.Name != "origin" || r.URL != "https://example.com/kb.git" || r.Branch != "main" || r.Interval != 300 {
		t.Fatalf("unexpected remote: %+v", r)
	}
	if r.PushInterval != 300 {
		t.Fatalf("expected push_interval=300, got: %d", r.PushInterval)
	}
	if r.LastSyncAt != nil || r.LastStatus != nil || r.LastError != nil {
		t.Fatal("expected nil sync status fields for new remote")
	}
	if r.LastPushAt != nil || r.LastPushStatus != nil || r.LastPushError != nil {
		t.Fatal("expected nil push status fields for new remote")
	}

	// UpdateRemoteStatus (pull) sets status.
	if err := svc.UpdateRemoteStatus("origin", "ok", nil); err != nil {
		t.Fatal(err)
	}
	r, err = svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r.LastStatus == nil || *r.LastStatus != "ok" {
		t.Fatalf("expected status=ok, got: %v", r.LastStatus)
	}
	if r.LastSyncAt == nil {
		t.Fatal("expected non-nil last_sync_at after status update")
	}

	// UpdateRemotePushStatus sets push status.
	if err := svc.UpdateRemotePushStatus("origin", "ok", nil); err != nil {
		t.Fatal(err)
	}
	r, err = svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r.LastPushStatus == nil || *r.LastPushStatus != "ok" {
		t.Fatalf("expected push status=ok, got: %v", r.LastPushStatus)
	}
	if r.LastPushAt == nil {
		t.Fatal("expected non-nil last_push_at after push status update")
	}

	// UpdateRemotePushStatus with error.
	errMsg := "permission denied"
	if err := svc.UpdateRemotePushStatus("origin", "error", &errMsg); err != nil {
		t.Fatal(err)
	}
	r, err = svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r.LastPushStatus == nil || *r.LastPushStatus != "error" {
		t.Fatalf("expected push status=error, got: %v", r.LastPushStatus)
	}
	if r.LastPushError == nil || *r.LastPushError != "permission denied" {
		t.Fatalf("expected push error='permission denied', got: %v", r.LastPushError)
	}

	// SetRemote with different values replaces.
	if err := svc.SetRemote("origin", "https://other.com/kb.git", "master", 60, 120); err != nil {
		t.Fatal(err)
	}
	r, err = svc.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://other.com/kb.git" || r.Branch != "master" || r.Interval != 60 || r.PushInterval != 120 {
		t.Fatalf("unexpected remote after replace: %+v", r)
	}
}
