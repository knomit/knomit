package cmd

import (
	"strings"
	"testing"

	"knomit/internal/config"
)

// The PIN on serve's half of the silent-lockout guard (knomit#245 review).
//
// The guard itself is tested in internal/auth; what could not be reached from
// there is whether SERVE actually consults [auth].require. It did not used to
// be reachable from anywhere: the value was one argument among several at the
// call site, and mutating it to a constant left every test in the tree green.
// openLocalListener takes the whole config precisely so this test can pin the
// mapping, and so there is no pair of arguments to mis-wire in the first
// place.
//
// cfg.Socket == "" is the cheapest way to reach "no listener was bound":
// ListenLocal answers (nil, noop, nil) for it on every platform, no fixture
// required. What happens when one IS bound, or when the path is held by
// another process, is covered against the real transport in internal/auth
// (TestRequireLocalListener, TestListenLocal_ForeignOwnerIsNotStolenAndMapsToInUse)
// and in tools/desktop (TestBootServer_PipeHeldWithRequireAuthFailsTheBoot).
// A full RunE test is not the cheap option here: serve reaches this point only
// after app.New, which needs the embedding model.
func TestOpenLocalListener_HonoursAuthRequire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require bool
		wantErr bool
	}{
		// The regression: require is on and nothing was bound.
		{"require, no listener", true, true},
		// Positive control. Without it the case above would pass for an
		// openLocalListener that refused every boot.
		{"no require, no listener", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Home = t.TempDir()
			cfg.Socket = "" // nothing to bind, on any platform
			cfg.Auth.Require = tc.require

			ln, cleanup, err := openLocalListener(cfg)
			if cleanup == nil {
				t.Fatal("cleanup must never be nil; the caller defers it unconditionally")
			}
			defer cleanup()
			if ln != nil {
				t.Fatalf("no listener can exist for an empty socket path, got %v", ln)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			// The operator has to be able to act on it. "= true" and not the
			// bare setting name: the remediation clause says "set
			// [auth].require = false", which a bare-name needle would match
			// on its own.
			if !strings.Contains(err.Error(), "[auth].require = true") {
				t.Fatalf("the refusal must name the setting that caused it; got: %v", err)
			}
		})
	}
}
