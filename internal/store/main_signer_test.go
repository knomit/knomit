package store

import (
	"crypto/ed25519"
	"crypto/sha256"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fallbackTestSigner is the same fixed key internal/testsupport/testsigner
// installs; this package cannot import that one (it imports store).
func fallbackTestSigner() ssh.Signer {
	seed := sha256.Sum256([]byte("knomit test fallback signer"))
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic(err)
	}
	return s
}

// TestMain installs the fallback commit signer for this test binary: a
// Service without SetSigner now refuses to commit (ErrNoSigner), and most
// fixtures here predate that. TestSign_NoSignerIsRefused removes it for its
// own duration.
//
// It also turns SQLite's fsyncs off for every DB the binary opens (#365), as
// internal/web does: a test that loses power mid-run has lost nothing worth
// keeping. WAL stays on, so locking behaves as in production. A test of
// durability itself must turn the switch off for its own duration; see
// SetTestFastDurability.
func TestMain(m *testing.M) {
	SetTestFallbackSigner(fallbackTestSigner())
	SetTestFastDurability(true)
	os.Exit(m.Run())
}
