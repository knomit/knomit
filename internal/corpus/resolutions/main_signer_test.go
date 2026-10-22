package resolutions

import (
	"os"
	"testing"

	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner refuses to commit (store.ErrNoSigner), and the
// fixtures here commit through a bare store.
func TestMain(m *testing.M) {
	testsigner.Install()
	os.Exit(m.Run())
}
