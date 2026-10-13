package pki

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain points go-keyring at its in-memory mock before any test runs, so
// nothing in this package's tests can reach the real OS keychain even
// through OSKeychain. The custody tests use fakeKeychain directly.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}
