//go:build !windows

package cmd

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// knomit#253: a local listener path too long for sun_path is a WARN and TCP
// only, never a failed boot — unless [auth].require makes TCP-only useless,
// in which case RequireLocalListener refuses the boot with the cause named.
// The overlong path is set EXPLICITLY (what [socket] / KNOMIT_SOCKET can
// still do); a long data root alone gets the short fallback and never
// reaches this branch.
//
// Sabotage (run against the commit that added it): deleting the
// ErrPathTooLong case sends the error down `case err != nil` and the
// no-require row fails with the error instead of nil.
func TestOpenLocalListener_PathTooLongWarnsAndServesTCPOnly(t *testing.T) {
	long := "/tmp/" + strings.Repeat("l", 120) + "/knomit.sock"
	for _, tc := range []struct {
		name    string
		require bool
	}{
		{"no require: warn, TCP only", false},
		{"require: refuse the boot, naming the cause", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			cfg := config.Defaults()
			cfg.Home = t.TempDir()
			cfg.Socket = long
			cfg.Auth.Require = tc.require

			ln, cleanup, err := openLocalListener(cfg)
			defer cleanup()
			if ln != nil {
				t.Fatal("a listener was bound on an overlong path")
			}
			if tc.require {
				if !errors.Is(err, auth.ErrPathTooLong) || !strings.Contains(err.Error(), "[auth].require = true") {
					t.Fatalf("err = %v, want the require refusal wrapping ErrPathTooLong", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil (WARN and serve TCP only)", err)
			}
			out := logs.String()
			if !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, long) ||
				!strings.Contains(out, "the cap on this platform is "+strconv.Itoa(auth.SunPathCap())) {
				t.Fatalf("want a WARN naming the path and the cap %d; got:\n%s", auth.SunPathCap(), out)
			}
		})
	}
}
