package synthesize

import (
	"os"
	"testing"

	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that.
func TestMain(m *testing.M) {
	testsigner.Install()
	os.Exit(m.Run())
}
