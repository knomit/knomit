//go:build desktop

package main

import (
	"testing"

	"knomit/internal/version"
)

// validTestKey is a syntactically valid base64 Ed25519 public key (32 zero
// bytes). Nothing is verified against it in these tests.
const validTestKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// setBuild swaps the injected build identity for the duration of a test.
func setBuild(t *testing.T, ver, key string) {
	t.Helper()
	origVer, origKey := version.Version, version.UpdatePublicKey
	t.Cleanup(func() { version.Version, version.UpdatePublicKey = origVer, origKey })
	version.Version, version.UpdatePublicKey = ver, key
}

// pkg/updater fails OPEN: runVerification returns nil for a release carrying
// no verification block, and Config has no "require a signature" switch. So a
// build with no pinned key must not run an updater at all — anything else is
// an unauthenticated update channel. Do not "simplify" this guard away.
func TestUpdaterConfigDisabledWithoutPinnedKey(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		version     string
		key         string
		wantEnabled bool
	}{
		{"no key disables the updater", "darwin", "0.5.0", "", false},
		{"dev build disables the updater", "darwin", "dev", validTestKey, false},
		{"dev build with no key is doubly disabled", "darwin", "dev", "", false},
		{"released darwin build with a key enables it", "darwin", "0.5.0", validTestKey, true},
		// pkg/updater is AppImage-unaware: os.Executable() resolves into the
		// FUSE mount, so the swap would replace the mount path instead of the
		// .AppImage file. This guard — not the absence of a linux feed item —
		// is what protects installed AppImages, because a feed is remote data
		// that can change long after the binary ships.
		{"linux is disabled even with a valid key", "linux", "0.5.0", validTestKey, false},
		{"windows is disabled", "windows", "0.5.0", validTestKey, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBuild(t, tt.version, tt.key)

			cfg, enabled, err := updaterConfig(tt.goos)
			if err != nil {
				t.Fatalf("updaterConfig: %v", err)
			}
			if enabled != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if !enabled {
				return
			}
			if cfg.CurrentVersion != tt.version {
				t.Errorf("CurrentVersion = %q, want %q", cfg.CurrentVersion, tt.version)
			}
			if len(cfg.PublicKey) == 0 {
				t.Error("PublicKey is empty on an enabled updater")
			}
			// Exactly one provider, by decision: a fallback provider that
			// yields unsigned releases would be a downgrade path, because
			// verification fails open on them.
			if len(cfg.Providers) != 1 {
				t.Errorf("len(Providers) = %d, want exactly 1", len(cfg.Providers))
			}
			if cfg.CheckInterval <= 0 {
				t.Error("CheckInterval must be positive for background checks")
			}
		})
	}
}

func TestUpdaterConfigRejectsMalformedKey(t *testing.T) {
	setBuild(t, "0.5.0", "!!! not base64 !!!")

	if _, _, err := updaterConfig("darwin"); err == nil {
		t.Error("updaterConfig accepted a malformed key, want an error")
	}
}

// CurrentVersion must be the bare semver. version.String() appends the commit
// SHA ("0.5.0.2a7ae9d"), which semver.IsNewer cannot parse — the update check
// would then never fire.
func TestUpdaterConfigUsesBareSemver(t *testing.T) {
	origCommit := version.Commit
	t.Cleanup(func() { version.Commit = origCommit })
	version.Commit = "2a7ae9d"
	setBuild(t, "0.5.0", validTestKey)

	cfg, enabled, err := updaterConfig("darwin")
	if err != nil || !enabled {
		t.Fatalf("updaterConfig: enabled=%v err=%v", enabled, err)
	}
	if cfg.CurrentVersion != "0.5.0" {
		t.Errorf("CurrentVersion = %q, want the bare semver 0.5.0", cfg.CurrentVersion)
	}
	if cfg.CurrentVersion == version.String() {
		t.Error("CurrentVersion is version.String(); semver.IsNewer cannot parse the SHA suffix")
	}
}

// The feed URL is compiled into every shipped binary and cannot be changed for
// clients already in the field, so it must match what the release workflow
// publishes to.
func TestUpdateFeedURLIsTheHTTPSPagesURL(t *testing.T) {
	const want = "https://knomit.github.io/knomit/appcast.xml"
	if updateFeedURL != want {
		t.Errorf("updateFeedURL = %q, want %q (must match release-stable.yml)", updateFeedURL, want)
	}
}
