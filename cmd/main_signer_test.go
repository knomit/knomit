package cmd

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"

	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that.
//
// It also points go-keyring at its in-memory mock: the identity commands
// keep the fleet root key in the OS keychain (rootKeychain), and no test may
// reach the real one.
func TestMain(m *testing.M) {
	testsigner.Install()
	keyring.MockInit()
	os.Exit(m.Run())
}
