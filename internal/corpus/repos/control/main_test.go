package control

import (
	"os"
	"testing"

	"knomit/internal/engine/store/storetest"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer and turns SQLite's fsyncs off
// for every DB the binary opens (#365), as the repos parent and
// internal/edge/web do. TestControlDB_HonoursTestFastDurability flips the
// switch for its own duration and restores this value.
func TestMain(m *testing.M) {
	testsigner.Install()
	storetest.SetFastDurability(true)
	os.Exit(m.Run())
}
