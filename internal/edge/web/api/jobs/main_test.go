package jobs

import (
	"os"
	"testing"

	"knomit/internal/engine/store/storetest"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer and turns SQLite's fsyncs off
// for this test binary, as the sibling api packages' do.
func TestMain(m *testing.M) {
	testsigner.Install()
	storetest.SetFastDurability(true)
	os.Exit(m.Run())
}
