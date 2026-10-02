package web

import (
	"os"
	"testing"

	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// TestMain installs the fallback commit signer for this test binary: a store
// opened without SetSigner now refuses to commit (store.ErrNoSigner), and the
// fixtures here predate that.
//
// It also turns SQLite's fsyncs off for every DB the binary opens (#365):
// ~400 tests here each build a repos.Manager and create real repos, and on
// Windows the fsyncs of each create dominated the package's run time (706s ->
// 389s on a Windows 10 laptop; see .github/workflows/tests.yml). WAL itself
// stays on, so locking behaves as in production; see
// store.SetTestFastDurability for what a durability test must do instead.
func TestMain(m *testing.M) {
	testsigner.Install()
	store.SetTestFastDurability(true)
	os.Exit(m.Run())
}
