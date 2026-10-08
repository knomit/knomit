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
//
// t.Parallel. A top-level test that calls t.Parallel waits until every test
// that does not has finished, and then runs beside the other parallel ones
// only. So the rule is about the parallel set alone: a test may call
// t.Parallel when nothing it reaches, its helpers included, changes
// process-wide state. That excludes setHooks and SetTriggerClockForTest
// (triggerTestHooks), the sync, consensus and recipe-exec hooks, the other
// package-level seams (swapCopy, hostPlatform, guidanceWarnHook,
// templatePresetByID, recipeServerAddrWait, experimentSweepIntervalOverride),
// captureLogs (log.Logger), t.Setenv, SetTestFastDurability and fact's
// trigger globals. It also excludes a test whose assertion is a latency or a
// short positive deadline (the write-latency gates,
// EmitReachesStreamWithoutDebounce, a 2 s Eventually): those keep the machine
// to themselves by staying sequential. The parallel tests are the slow,
// self-contained ones from the Windows per-test data (#365 follow-up); a
// quick test gains nothing from it.
func TestMain(m *testing.M) {
	maybeRunRecipeHelper()
	testsigner.Install()
	store.SetTestFastDurability(true)
	os.Exit(m.Run())
}
