package repos

import (
	"os"
	"testing"

	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that. A process started by a recipe under test (the
// test binary re-executed with KNOMIT_RECIPE_HELPER set) becomes the helper
// instead and never runs the tests.
//
// It also turns SQLite's fsyncs off for every DB the binary opens (#365), as
// internal/web does: a test that loses power mid-run has lost nothing worth
// keeping. WAL stays on, so locking behaves as in production. A test of
// durability itself must turn the switch off for its own duration; see
// store.SetTestFastDurability.
func TestMain(m *testing.M) {
	maybeRunRecipeHelper()
	testsigner.Install()
	store.SetTestFastDurability(true)
	os.Exit(m.Run())
}
