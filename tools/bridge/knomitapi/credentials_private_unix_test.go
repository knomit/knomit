//go:build !windows

package knomitapi

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// `kb login` can be the first thing ever run on a machine, so SaveCredentials
// makes the data root private itself (ensureCredentialsDir). The root is
// pre-created 0755, as the earliest writers used to leave it: a fresh one
// would come out 0700 from MkdirAll alone and prove nothing.
func TestSaveCredentials_MakesTheDataRootPrivate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	isolateHomeEnv(t, home)

	base, _ := url.Parse("https://example.com")
	if err := SaveCredentials(base, &Credentials{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o700 {
		t.Errorf("data root after SaveCredentials has mode %v, want 0700", m)
	}
}
