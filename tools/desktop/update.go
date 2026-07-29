//go:build desktop

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/appcast"

	"knomit/internal/version"
)

// updateFeedURL is the published Sparkle appcast the desktop app polls. It is
// COMPILED IN and cannot be changed for binaries already in the field, so it
// must stay stable for the life of the update channel and must match what
// .github/workflows/release-stable.yml publishes to.
const updateFeedURL = "https://knomit.github.io/knomit/appcast.xml"

// updateCheckInterval is how often the app polls the feed in the background.
// The app is long-lived — it sits in the tray — so a slow cadence still
// reaches everyone within a day.
const updateCheckInterval = 6 * time.Hour

// updaterConfig builds the updater configuration and reports whether
// self-update should run at all. goos is a parameter rather than a direct
// runtime.GOOS read so the platform guard can be tested on any host.
//
// Self-update is DISABLED in three cases, each a guard rather than a
// convenience:
//
//   - Not darwin. pkg/updater is AppImage-unaware: os.Executable() resolves
//     into the FUSE mount, so bundleTarget targets the mount rather than the
//     .AppImage file and the swap replaces the wrong path. This check is what
//     protects installed AppImages — NOT the fact that we currently publish
//     no linux feed item. A feed is remote data and can change after the
//     binary ships; this cannot.
//   - Dev builds, so a local `make` build never joins a live update channel.
//   - No pinned public key. pkg/updater's runVerification returns nil for a
//     release carrying no verification block, and Config offers no way to
//     demand a signature — so an updater without a pinned key would install
//     unsigned artifacts silently. "No key" must mean "no updater".
func updaterConfig(goos string) (updater.Config, bool, error) {
	if goos != "darwin" || version.Version == "dev" || version.UpdatePublicKey == "" {
		return updater.Config{}, false, nil
	}

	pub, err := base64.StdEncoding.DecodeString(version.UpdatePublicKey)
	if err != nil {
		return updater.Config{}, false, fmt.Errorf("decode update public key: %w", err)
	}

	prov, err := appcast.New(appcast.Config{URL: updateFeedURL})
	if err != nil {
		return updater.Config{}, false, fmt.Errorf("appcast provider: %w", err)
	}

	return updater.Config{
		// The BARE semver. version.String() appends the commit SHA, which
		// semver.IsNewer cannot parse — the check would never fire.
		CurrentVersion: version.Version,
		// Exactly one provider. A fallback would be a downgrade path: knock
		// the feed host offline and clients would accept unsigned artifacts
		// from the fallback, because verification fails open on them.
		Providers:     []updater.Provider{prov},
		PublicKey:     pub,
		CheckInterval: updateCheckInterval,
		Window:        &updater.BuiltinWindow{},
	}, true, nil
}

// configureUpdater wires self-update into the running app and reports whether
// it is live. A failure here is logged and swallowed by the caller: the app
// must start even when updates cannot.
func configureUpdater(wapp *application.App) (bool, error) {
	cfg, enabled, err := updaterConfig(runtime.GOOS)
	if err != nil {
		return false, err
	}
	if !enabled {
		log.Info().Str("goos", runtime.GOOS).
			Msg("self-update disabled (non-darwin, dev build, or no pinned update key)")
		return false, nil
	}

	if err := wapp.Updater.Init(cfg); err != nil {
		return false, fmt.Errorf("updater init: %w", err)
	}

	// A background check that fails silently is a channel nobody notices has
	// stopped working — surface it in the log file. The updater emits
	// ErrorInfo as the sole Emit argument, which EventManager.Emit assigns
	// straight to CustomEvent.Data, so a Go subscriber sees the concrete
	// struct; the fallback covers a payload that has round-tripped through
	// JSON.
	wapp.Event.On(updater.EventError, func(e *application.CustomEvent) {
		if info, ok := e.Data.(updater.ErrorInfo); ok {
			log.Warn().
				Str("stage", string(info.Stage)).
				Str("provider", info.Provider).
				Str("error", info.Message).
				Msg("update failed")
			return
		}
		log.Warn().Str("event", e.ToJSON()).Msg("update failed")
	})

	log.Info().Str("feed", updateFeedURL).Str("version", cfg.CurrentVersion).
		Msg("self-update enabled")
	return true, nil
}

// updateRunner is the slice of *updater.Updater the tray menu item needs.
// Narrowing it to an interface is what makes the method choice below testable
// — the bug this replaced was a wrong method call, not wrong logic.
type updateRunner interface {
	CheckAndInstall(context.Context) error
}

// checkForUpdatesNow runs an on-demand check for the tray menu item.
//
// It MUST call CheckAndInstall, not Check. Check only walks the providers and
// emits events; the updater's window is opened exclusively by CheckAndInstall
// (pkg/updater/updater.go — openSession has one caller), so a bare Check
// produces no prompt, no "you're up to date" panel and no install, whatever it
// finds. That is the do-nothing button the menu entry exists to avoid.
//
// No timeout on the context: CheckAndInstall spans the download and install of
// a whole app bundle, so any deadline short enough to be useful for the check
// would abort the install. Cancellation comes from the window's Cancel button.
func checkForUpdatesNow(u updateRunner) {
	go func() {
		if err := u.CheckAndInstall(context.Background()); err != nil {
			log.Warn().Err(err).Msg("manual update check failed")
		}
	}()
}
