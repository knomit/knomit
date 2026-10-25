package trigger

import (
	"os"
	"testing"

	"knomit/internal/engine/store/storetest"
	"knomit/internal/testsupport/testsigner"
)

// TestMain is the repos parent's, for this child: a process started by an
// exec test (the test binary re-executed with recipeHelperEnv set) becomes
// the helper and never runs the tests; otherwise the fallback commit signer
// is installed and SQLite's fsyncs are off, as in every package whose tests
// may open a store.
//
// The trigger tests that need a repo (a commit kicks the dispatcher through
// ri.onCommit) live in the parent, which has its own recipe helper; the tests
// here drive this package's exec layer directly.
func TestMain(m *testing.M) {
	maybeRunRecipeHelper()
	testsigner.Install()
	storetest.SetFastDurability(true)
	os.Exit(m.Run())
}
