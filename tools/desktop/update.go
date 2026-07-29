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
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/appcast"

	"knomit/internal/version"
)

// updateFeedURL is the published Sparkle appcast the desktop app polls. It is
// COMPILED IN and cannot be changed for binaries already in the field, so it
// must stay stable for the life of the update channel and must match what
// .github/workflows/release-stable.yml publishes to.
const updateFeedURL = "https://knomit.github.io/knomit/appcast.xml"

// updateCheckInterval is how often pollForUpdates polls the feed. The app is
// long-lived — it sits in the tray — so a slow cadence still reaches everyone
// within a day.
const updateCheckInterval = 6 * time.Hour

// Notification identity. The category is what carries the "Install & Restart"
// action button; a notification naming an unregistered category still
// delivers, silently losing its buttons, so registration has to happen before
// the first send rather than lazily alongside it.
const (
	updateCategoryID      = "knomit.update.available"
	updateNotificationID  = "knomit.update.available"
	updateInstallActionID = "install"
)

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
		Providers: []updater.Provider{prov},
		PublicKey: pub,
		// No window, and NO CheckInterval. The two go together.
		//
		// pkg/updater's periodicCheckLoop calls CheckAndInstall, not Check.
		// CheckAndInstall opens its window BEFORE it knows whether an update
		// exists (openSession runs ahead of the Check) and then deliberately
		// leaves it open on the up-to-date path to avoid a flicker. With a
		// builtin window that is an uninvited "You're Up to Date" panel every
		// interval, forever, on an app that sits in the tray for weeks.
		// pkg/updater says so itself: apps wanting silent background polling
		// should use WindowNone or drive Check directly.
		//
		// So we do both. WindowNone means no window can ever appear, and our
		// own ticker (pollForUpdates) calls Check, which reports without
		// installing. Setting CheckInterval as well would re-arm the built-in
		// loop, and a HEADLESS CheckAndInstall installs with no prompt at all
		// — strictly worse than the window it replaced.
		Window: updater.WindowNone,
	}, true, nil
}

// updateChecker reports whether a newer release exists, without installing it.
type updateChecker interface {
	Check(context.Context) (*updater.Release, error)
}

// updateInstaller performs the install and the relaunch.
//
// Installing means CheckAndInstall, never Check: Check only walks the
// providers and emits events, so a bare Check downloads and installs nothing
// whatever it finds. Keeping these two interfaces separate is what makes that
// enforceable — the polling path is handed something that CANNOT install, and
// the install path something that cannot merely look.
type updateInstaller interface {
	CheckAndInstall(context.Context) error
	Restart(context.Context) error
}

// updateNotifier is the slice of *notifications.NotificationService this file
// needs, narrowed so the banner payloads are assertable in tests without a
// running notification centre.
type updateNotifier interface {
	RegisterNotificationCategory(notifications.NotificationCategory) error
	SendNotificationWithActions(notifications.NotificationOptions) error
}

// registerUpdateCategory declares the action button the update banner carries.
func registerUpdateCategory(n updateNotifier) error {
	return n.RegisterNotificationCategory(notifications.NotificationCategory{
		ID: updateCategoryID,
		Actions: []notifications.NotificationAction{
			{ID: updateInstallActionID, Title: "Install & Restart"},
		},
	})
}

// notifyUpdateAvailable posts the OS banner announcing rel.
//
// The body states the consequence, not just the availability: installing may
// migrate the knowledge base and migrations do not reverse, so the click this
// asks for has to be an informed one.
func notifyUpdateAvailable(n updateNotifier, rel *updater.Release, current string) error {
	return n.SendNotificationWithActions(notifications.NotificationOptions{
		// A stable ID, so a version left unclaimed for days coalesces into
		// one banner in Notification Centre instead of stacking a new one on
		// every poll.
		ID:         updateNotificationID,
		Title:      fmt.Sprintf("Knomit %s is available", rel.Version),
		Subtitle:   fmt.Sprintf("You're on %s", current),
		Body:       "Installing restarts Knomit and may migrate your knowledge base. Migrations do not reverse.",
		CategoryID: updateCategoryID,
		Data:       map[string]any{"version": rel.Version},
	})
}

// checkAndNotify runs one check and posts a banner when an update exists.
//
// SILENT when already current, by decision: the banner is the only update UI
// there is, and a "you're up to date" interruption every interval is exactly
// what the windowless design exists to avoid. The tray item shares this path,
// so a manual check that finds nothing is silent too.
func checkAndNotify(ctx context.Context, c updateChecker, n updateNotifier, current string) error {
	rel, err := c.Check(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		log.Debug().Str("version", current).Msg("update check: already current")
		return nil
	}
	log.Info().Str("available", rel.Version).Str("current", current).Msg("update available")
	return notifyUpdateAvailable(n, rel, current)
}

// pollForUpdates checks on a fixed cadence until ctx is cancelled. It only
// ever reports; installing is gated behind the banner's action button.
func pollForUpdates(ctx context.Context, c updateChecker, n updateNotifier, current string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := checkAndNotify(ctx, c, n, current); err != nil {
				// A background check that fails silently is a channel nobody
				// notices has stopped working — surface it in the log file.
				log.Warn().Err(err).Msg("background update check failed")
			}
		}
	}
}

// installAndRestart downloads, verifies, installs and relaunches.
//
// No timeout on the context: this spans the download and install of a whole
// app bundle, so any deadline short enough to bound the network check would
// abort a legitimate install.
//
// Restart runs only if the install succeeded. pkg/updater's Restart returns
// ErrNotReady when no artifact was staged, and quitting the app on a failed
// install would be indistinguishable from a crash.
func installAndRestart(ctx context.Context, u updateInstaller) error {
	if err := u.CheckAndInstall(ctx); err != nil {
		return fmt.Errorf("install update: %w", err)
	}
	return u.Restart(ctx)
}

// configureUpdater wires self-update into the running app: it initialises the
// updater, registers the notification category and its response handler, and
// starts the background poll.
//
// A failure here is logged and swallowed by the caller: the app must start
// even when updates cannot.
func configureUpdater(
	ctx context.Context,
	wapp *application.App,
	notify *notifications.NotificationService,
	cfg updater.Config,
) error {
	if err := wapp.Updater.Init(cfg); err != nil {
		return fmt.Errorf("updater init: %w", err)
	}

	// Best-effort: an unsigned bundle, or a user who has denied Knomit
	// notification permission, leaves the updater running and logging while
	// posting no banners. A degraded channel, not a broken app.
	if granted, err := notify.RequestNotificationAuthorization(); err != nil {
		log.Warn().Err(err).Msg("update notifications unavailable")
	} else if !granted {
		log.Warn().Msg("update notifications not permitted; updates will appear only in the log")
	}

	if err := registerUpdateCategory(notify); err != nil {
		log.Warn().Err(err).Msg("update notification action button unavailable")
	}

	// The banner's action button is the ONLY thing that installs. Anything
	// that is not that action — a dismissal, a click on the body — is
	// deliberately a no-op, so a stray click cannot start a migration.
	notify.OnNotificationResponse(func(res notifications.NotificationResult) {
		if res.Error != nil {
			log.Warn().Err(res.Error).Msg("update notification response failed")
			return
		}
		if res.Response.ActionIdentifier != updateInstallActionID {
			return
		}
		go func() {
			if err := installAndRestart(ctx, wapp.Updater); err != nil {
				log.Warn().Err(err).Msg("update install failed")
			}
		}()
	})

	// The updater emits ErrorInfo as the sole Emit argument, which
	// EventManager.Emit assigns straight to CustomEvent.Data, so a Go
	// subscriber sees the concrete struct; the fallback covers a payload that
	// has round-tripped through JSON.
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

	go pollForUpdates(ctx, wapp.Updater, notify, cfg.CurrentVersion, updateCheckInterval)

	log.Info().Str("feed", updateFeedURL).Str("version", cfg.CurrentVersion).
		Dur("interval", updateCheckInterval).Msg("self-update enabled")
	return nil
}

// checkForUpdatesNow runs an on-demand check for the tray menu item, on a
// goroutine so the click returns immediately. Same path as the background
// poll: it reports, it does not install.
func checkForUpdatesNow(ctx context.Context, c updateChecker, n updateNotifier, current string) {
	go func() {
		if err := checkAndNotify(ctx, c, n, current); err != nil {
			log.Warn().Err(err).Msg("manual update check failed")
		}
	}()
}

// selfUpdateDisabledReason names why self-update is off, so a build that
// unexpectedly has no update channel is diagnosable from the log file alone.
func selfUpdateDisabledReason() string {
	switch {
	case runtime.GOOS != "darwin":
		return "self-update is macOS-only"
	case version.Version == "dev":
		return "dev build"
	case version.UpdatePublicKey == "":
		return "no pinned update key"
	}
	return "unknown"
}
