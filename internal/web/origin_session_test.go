package web

import (
	"os"
	"testing"
	"time"
)

func TestSessionManager_CreateAndCleanup(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Shutdown()

	// Create first session for repo "alpha"
	s1, err := sm.Create("alpha", "https://example.com/alpha.git", AuthConfig{Method: "token", Token: "abc"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Temp dir should exist
	if _, err := os.Stat(s1.TempDir); err != nil {
		t.Fatalf("temp dir %q should exist: %v", s1.TempDir, err)
	}

	firstTempDir := s1.TempDir
	firstID := s1.ID

	// Create second session for the same repo — old one must be cleaned up
	s2, err := sm.Create("alpha", "https://example.com/alpha.git", AuthConfig{Method: "token", Token: "xyz"})
	if err != nil {
		t.Fatalf("second Create failed: %v", err)
	}

	// Old temp dir should be gone
	if _, err := os.Stat(firstTempDir); !os.IsNotExist(err) {
		t.Errorf("old temp dir %q should have been removed", firstTempDir)
	}

	// New session should have a different ID
	if s2.ID == firstID {
		t.Errorf("expected new session ID, got same: %s", firstID)
	}

	// New temp dir should exist
	if _, err := os.Stat(s2.TempDir); err != nil {
		t.Fatalf("new temp dir %q should exist: %v", s2.TempDir, err)
	}
}

func TestSessionManager_GetByID(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Shutdown()

	s, err := sm.Create("beta", "https://example.com/beta.git", AuthConfig{Method: "token", Token: "tok"})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Correct repo + correct ID should succeed
	got, ok := sm.Get("beta", s.ID)
	if !ok {
		t.Fatal("Get with correct id should return ok=true")
	}
	if got.ID != s.ID {
		t.Errorf("Get returned wrong session: want %s, got %s", s.ID, got.ID)
	}

	// Wrong ID should fail
	_, ok = sm.Get("beta", "nonexistent-id")
	if ok {
		t.Error("Get with wrong id should return ok=false")
	}

	// Wrong repo should fail
	_, ok = sm.Get("wrong-repo", s.ID)
	if ok {
		t.Error("Get with wrong repo should return ok=false")
	}
}

func TestSessionManager_Delete(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Shutdown()

	s, err := sm.Create("gamma", "https://example.com/gamma.git", AuthConfig{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	tmpDir := s.TempDir

	sm.Delete("gamma", s.ID)

	// Temp dir should be gone
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q should have been removed after Delete", tmpDir)
	}

	// Get should return false
	_, ok := sm.Get("gamma", s.ID)
	if ok {
		t.Error("Get after Delete should return ok=false")
	}
}

func TestSessionManager_ExpiresAfterTimeout(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Shutdown()

	s, err := sm.Create("delta", "https://example.com/delta.git", AuthConfig{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	tmpDir := s.TempDir
	id := s.ID

	// Wind back LastAccess to simulate idle > 10 minutes
	s.mu.Lock()
	s.LastAccess = time.Now().Add(-11 * time.Minute)
	s.mu.Unlock()

	// Manually trigger cleanup
	sm.runCleanup()

	// Session should be gone
	_, ok := sm.Get("delta", id)
	if ok {
		t.Error("expired session should have been cleaned up")
	}

	// Temp dir should be gone
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q should have been removed after expiry", tmpDir)
	}
}
