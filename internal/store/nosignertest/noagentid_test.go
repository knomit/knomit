package nosignertest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"knomit/internal/store"
	"knomit/internal/store/internal/vcs"
)

// TestNoAgentID_AuthoredWriteIsRefused (D18): this binary installs no test
// fallbacks, so it sees what production sees. A store with a signer but no
// agent id refuses to author a commit rather than deriving an id from the
// branch name ("agent/test-deadbeef" would have given "test-deadbeef").
func TestNoAgentID_AuthoredWriteIsRefused(t *testing.T) {
	svc, err := store.Open(filepath.Join(t.TempDir(), "repo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()
	if err := svc.InitRepo(ctx, map[string]string{"README.md": "seed"}, "agent/test-deadbeef"); err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("nosignertest"))
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSigner(signer)
	if vcs.TestFallbackAgentForTest() != nil {
		t.Fatal("fixture: this binary must have no fallback agent id installed")
	}
	_, err = svc.Facts().WriteFact(ctx, "agent/test-deadbeef", "kb/notes/a.md", factBody, "learn: a", "created")
	if !errors.Is(err, vcs.ErrNoAgentID) {
		t.Fatalf("WriteFact without an agent id: got %v, want ErrNoAgentID", err)
	}
}
