package repos

import (
	"os"
	"testing"

	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that. A process started by a recipe under test (the
// test binary re-executed with KNOMIT_RECIPE_HELPER set) becomes the helper
// instead and never runs the tests.
func TestMain(m *testing.M) {
	maybeRunRecipeHelper()
	testsigner.Install()
	os.Exit(m.Run())
}
