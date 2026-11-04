package repos

import (
	"os"
	"testing"

	"knomit/internal/engine/store/storetest"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that. A process started by a recipe under test (the
// test binary re-executed with KNOMIT_RECIPE_HELPER set) becomes the helper
// instead and never runs the tests.
//
// It also turns SQLite's fsyncs off for every DB the binary opens (#365), as
// internal/edge/web does: a test that loses power mid-run has lost nothing worth
// keeping. WAL stays on, so locking behaves as in production. A test of
// durability itself must turn the switch off for its own duration; see
// storetest.SetFastDurability.
//
// t.Parallel. A top-level test that calls t.Parallel waits until every test
// that does not has finished, and then runs beside the other parallel ones
// only. So the rule is about the parallel set alone: a test may call
// t.Parallel when nothing it reaches, its helpers included, changes
// process-wide state. The general rule: any package-level variable that a
// test assigns, directly or through a setter, is a process-wide seam. So is
// any global of another package that a test assigns. The list below gives
// examples and is NOT complete; check what a test reaches, not only this
// list. Examples: setHooks and SetMissionTriggerHooksForTest
// (triggerTestHooks); the sync, consensus and recipe-exec hooks; the fleet
// hooks (fleetTestHooks via setFleetHooks, fleetRenewTestHooks,
// SetFleetClockForTest); the origin-write seams (beforeCredentialWrite,
// afterCredentialReread, beforeAttachOriginWrite, beforeDetachOriginWrite,
// beforeSwapOriginWrite); swapCopy, hostPlatform, guidanceWarnHook
// (SetGuidanceWarnHookForTest), templatePresetByID, recipeServerAddrWait and
// experimentSweepIntervalOverride; captureLog/captureLogs (log.Logger);
// t.Setenv; storetest.SetFastDurability; and fact's trigger globals. It also
// excludes a test whose assertion is a latency or a
// short positive deadline (the write-latency gates,
// EmitReachesStreamWithoutDebounce, a 2 s Eventually): those keep the machine
// to themselves by staying sequential. Moving objects over a git transport
// (clone, subscribe, fetch, push, probe, a served smart-HTTP clone) is NOT a
// reason to stay sequential. Those tests were sequential from #445 until #446,
// because concurrent go-git transfers under -race corrupted packs. The cause
// was sha1cd v0.6.0's arm64 assembly, which read an uninitialised register and
// sometimes returned a wrong SHA-1. It was fixed in sha1cd v0.7.0 (go-git
// v5.19.3). If such corruption comes back, check the sha1cd version before
// serialising tests. The parallel tests are the slow,
// self-contained ones from the Windows per-test data (#365 follow-up); a
// quick test gains nothing from it.
func TestMain(m *testing.M) {
	maybeRunRecipeHelper()
	testsigner.Install()
	storetest.SetFastDurability(true)
	os.Exit(m.Run())
}
