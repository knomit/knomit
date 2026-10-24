package origins

import (
	"os"
	"testing"

	"knomit/internal/engine/store/storetest"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that.
//
// It also turns SQLite's fsyncs off for every DB the binary opens, as web's
// TestMain does (#365, where the fsyncs of web's several hundred repo-creating
// tests dominated its Windows run time). This package has only a handful of
// tests, but the commit tests create real repos and clone real remotes, so
// the same setting keeps them off the fsync path. WAL itself stays on, so
// locking behaves as in production; see storetest.SetFastDurability for what
// a durability test must do instead.
func TestMain(m *testing.M) {
	testsigner.Install()
	storetest.SetFastDurability(true)
	os.Exit(m.Run())
}
