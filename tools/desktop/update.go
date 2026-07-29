//go:build desktop

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/appcast"

	"knomit/internal/version"
	"knomit/tools/desktop/internal/paths"
	"knomit/tools/desktop/internal/updatestate"
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

// initialCheckDelay is how long after startup the FIRST check runs.
//
// There has to be one. The tray badge is a passive surface, so with only the
// interval a freshly launched app shows nothing for six hours — and a user who
// launches, works and quits inside that window never learns an update exists
// at all.
//
// Delayed rather than immediate only to stay clear of server boot and the
// embedder load. Nothing depends on the exact value; it runs after
// activateNotifications either way, so the first banner cannot race the
// permission prompt.
const initialCheckDelay = 30 * time.Second

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

// updateAuthorizer adds the permission prompt to updateNotifier. It is a
// separate interface because the prompt is the one call in this file that can
// block for minutes, and separating it keeps activateNotifications — the only
// thing allowed to make it — explicit about what it touches.
type updateAuthorizer interface {
	updateNotifier
	RequestNotificationAuthorization() (bool, error)
}

// updateAnnouncer surfaces a found release OUTSIDE the notification centre:
// the tray icon badge and the tray menu item.
//
// It exists because the banner is not a reliable channel. knomit's .app is
// unsigned, and pkg/services/notifications documents "packaged and signed" as
// its requirement, so authorization can be denied or the whole service
// unavailable. With the banner as the only surface a user in that state has a
// running updater, a published release, and no way to learn that either
// exists. The badge makes the update discoverable; the menu item makes it
// installable.
//
// The tray is persistent STATE: AnnounceUpdate is called on EVERY check and
// is responsible for its own idempotence. Whether the banner fires is a
// separate question, answered by notifyLog — the two surfaces have different
// lifetimes and must not share one decision.
type updateAnnouncer interface {
	AnnounceUpdate(rel *updater.Release)
}

// notifyLog remembers which version the user has already been shown a banner
// for. It is what makes the banner a one-time EVENT rather than a recurring
// one, and it survives a restart — without that, "once per version" degrades
// to "once per version per launch".
type notifyLog interface {
	AlreadyNotified(version string) bool
	MarkNotified(version string) error
}

// persistentNotifyLog is the on-disk notifyLog, backed by
// <StateDir>/update.json.
//
// The value is cached in memory and the file is written only on a change, so
// the 6-hourly poll does not rewrite an identical file forever.
type persistentNotifyLog struct {
	path string

	mu   sync.Mutex
	last string
}

// newNotifyLog loads the banner memory from disk.
//
// Every failure degrades to an EMPTY log rather than an error. An unresolvable
// state dir, a missing file and a corrupt one all mean "nothing has been
// notified", which costs one redundant banner — the exact annoyance this state
// exists to prevent, and still far better than suppressing an update because a
// JSON file could not be read. A path that could not be resolved also makes
// MarkNotified a no-op, so the app degrades to the previous session-scoped
// behaviour instead of erroring on every check.
func newNotifyLog() *persistentNotifyLog {
	path, err := paths.UpdateStatePath()
	if err != nil {
		log.Warn().Err(err).Msg("update state path unavailable; banners will repeat each launch")
		return &persistentNotifyLog{}
	}
	return loadNotifyLog(path)
}

// loadNotifyLog reads the memory at path. Split from newNotifyLog so the load
// — the half that makes the state survive a restart at all — is reachable in a
// test without depending on the host's real state directory.
func loadNotifyLog(path string) *persistentNotifyLog {
	return &persistentNotifyLog{path: path, last: updatestate.Load(path).LastNotified}
}

func (l *persistentNotifyLog) AlreadyNotified(version string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return version != "" && version == l.last
}

func (l *persistentNotifyLog) MarkNotified(version string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" || version == l.last {
		return nil
	}
	l.last = version
	return updatestate.Save(l.path, updatestate.State{LastNotified: version})
}

// pendingUpdate is the release the most recent check found, shared between the
// poll goroutine that discovers it and the tray item that installs it.
//
// It is never cleared. A later check returning nil means "nothing newer than
// what is installed" — but it is also what a feed that is briefly empty,
// misconfigured or unreachable produces, and retracting a badge the user has
// already seen would turn a transient feed problem into a vanished update.
type pendingUpdate struct {
	mu  sync.Mutex
	rel *updater.Release
}

// set records rel and reports whether its version had not been seen before.
//
// Keyed on the VERSION, not on pointer identity: every poll builds a fresh
// *updater.Release, so comparing pointers would report every check as new and
// re-banner the same version forever — the exact behaviour this return exists
// to stop.
func (p *pendingUpdate) set(rel *updater.Release) bool {
	if rel == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rel != nil && p.rel.Version == rel.Version {
		return false
	}
	p.rel = rel
	return true
}

func (p *pendingUpdate) get() *updater.Release {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rel
}

// trayUpdateAnnouncer is the concrete updateAnnouncer: it badges the tray icon
// and turns the tray's "Check for Updates…" item into the install button.
type trayUpdateAnnouncer struct {
	pending *pendingUpdate
	icon    *trayIconState
	item    *application.MenuItem
	menu    *application.Menu
	tray    *application.SystemTray
}

func (t *trayUpdateAnnouncer) AnnounceUpdate(rel *updater.Release) {
	if rel == nil || !t.pending.set(rel) {
		// Same version the tray is already showing. The badge and the item
		// say so already, so re-running the AppKit calls every interval would
		// write identical strings for nothing.
		return
	}
	t.icon.setUpdateAvailable(true)
	t.tray.SetTooltip(fmt.Sprintf("Knomit %s is available", rel.Version))

	// MenuItem.SetLabel reaches AppKit on the CALLING goroutine — unlike
	// SystemTray.SetIcon/SetTooltip, which marshal themselves via InvokeSync —
	// and this method runs on the update poll goroutine. Hop to the main
	// thread explicitly rather than mutating a menu off it.
	application.InvokeSync(func() {
		t.item.SetLabel(fmt.Sprintf("Install Knomit %s & Restart…", rel.Version))
		t.menu.Update()
	})
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

// checkAndNotify runs one check, badges the tray and posts a banner when an
// update exists.
//
// SILENT when already current, by decision: a "you're up to date"
// interruption every interval is exactly what the windowless design exists to
// avoid. The tray item shares this path, so a manual check that finds nothing
// is silent too.
//
// The announcement comes BEFORE the notification and its error is not
// propagated, because the two have opposite reliability. The tray surfaces are
// in-process and cannot fail; the notification centre can refuse the app
// outright. Announcing second — or letting a failed banner skip the badge —
// would make the reliable surface contingent on the unreliable one.
//
// The banner fires ONCE PER VERSION, across launches, gated on notifyLog.
// Every check keeps refreshing the tray; only a version the user has not yet
// been told about reaches the notification centre. Without that gate an update
// left unclaimed re-banners every interval forever, which is the recurring
// interruption the windowless design exists to remove — merely relocated from
// a panel to Notification Centre.
//
// MarkNotified runs only after the banner was ACCEPTED. A send that failed
// reached nobody, so recording it would burn the one announcement that version
// gets; leaving it unmarked means a user who grants notification permission
// later still gets told. The retry is silent either way — a rejected send
// posts nothing — so this cannot become nagging.
func checkAndNotify(
	ctx context.Context,
	c updateChecker,
	n updateNotifier,
	a updateAnnouncer,
	seen notifyLog,
	current string,
) error {
	rel, err := c.Check(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		log.Debug().Str("version", current).Msg("update check: already current")
		return nil
	}

	// The tray first, and unconditionally: it is in-process and cannot fail,
	// where the notification centre can refuse the app outright. Ordering it
	// after — or gating it on the banner — would make the reliable surface
	// contingent on the unreliable one.
	a.AnnounceUpdate(rel)

	if seen.AlreadyNotified(rel.Version) {
		log.Debug().Str("available", rel.Version).Msg("update already announced; tray still shows it")
		return nil
	}
	log.Info().Str("available", rel.Version).Str("current", current).Msg("update available")
	if err := notifyUpdateAvailable(n, rel, current); err != nil {
		return err
	}
	if err := seen.MarkNotified(rel.Version); err != nil {
		// Non-fatal: the banner was delivered, so the user has been told. The
		// only consequence is that the next launch tells them again.
		log.Warn().Err(err).Msg("could not record the announced version; the banner will repeat next launch")
	}
	return nil
}

// pollForUpdates checks once after first, then on the every cadence, until ctx
// is cancelled. It only ever reports; installing is gated behind the banner's
// action button or the tray item.
//
// The initial check is what makes the tray badge useful. Ticker-only, a
// freshly launched app surfaces nothing until the first interval elapses, and
// a session shorter than that never learns an update exists.
func pollForUpdates(
	ctx context.Context,
	c updateChecker,
	n updateNotifier,
	a updateAnnouncer,
	seen notifyLog,
	current string,
	first, every time.Duration,
) {
	check := func() {
		if err := checkAndNotify(ctx, c, n, a, seen, current); err != nil {
			// A background check that fails silently is a channel nobody
			// notices has stopped working — surface it in the log file.
			log.Warn().Err(err).Msg("background update check failed")
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(first):
		check()
	}

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
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

// activateNotifications performs the notification-centre setup: the permission
// prompt and the category that carries the banner's action button.
//
// MUST NOT run before the application is up — see configureUpdater. Both steps
// are best-effort: an unsigned bundle, or a user who has denied Knomit
// notification permission, leaves the updater checking and the tray badging
// while posting no banners. A degraded channel, not a broken app.
//
// The category is registered even when authorization was refused. Permission
// can be granted later from System Settings without restarting Knomit, and a
// banner naming an unregistered category still delivers — silently losing its
// buttons, which is the one failure the user cannot diagnose.
func activateNotifications(n updateAuthorizer) {
	if granted, err := n.RequestNotificationAuthorization(); err != nil {
		log.Warn().Err(err).Msg("update notifications unavailable")
	} else if !granted {
		log.Warn().Msg("update notifications not permitted; updates will appear in the tray only")
	}

	if err := registerUpdateCategory(n); err != nil {
		log.Warn().Err(err).Msg("update notification action button unavailable")
	}
}

// configureUpdater wires self-update into the app and returns the half of that
// wiring that must wait until the application is running. The caller runs it
// from events.Common.ApplicationStarted.
//
// The split is not cosmetic. Everything done HERE is pure Go — updater
// initialisation and callback registration — and is safe before
// application.Run. Everything in the returned func reaches the OS notification
// centre, and on macOS that means UNUserNotificationCenter, which must not be
// touched before NSApplication exists:
//
//   - The notifications service's own guards — bundle identifier present,
//     delegate initialised — live in its ServiceStartup, and Wails runs
//     service startup from inside Run. Calling in earlier does not merely skip
//     them; [UNUserNotificationCenter currentNotificationCenter] raises rather
//     than returns when the main bundle is not yet resolvable, and an ObjC
//     exception through cgo takes the process with it. The app would die at
//     launch where it is supposed to log "self-update unavailable".
//   - RequestNotificationAuthorization blocks for up to THREE MINUTES waiting
//     on a completion handler that cannot arrive without a run loop. On the
//     startup path that is three minutes with no window and no tray icon.
//
// A failure here is logged and swallowed by the caller: the app must start
// even when updates cannot.
func configureUpdater(
	ctx context.Context,
	wapp *application.App,
	notify *notifications.NotificationService,
	announce updateAnnouncer,
	seen notifyLog,
	cfg updater.Config,
) (start func(), err error) {
	if err := wapp.Updater.Init(cfg); err != nil {
		return nil, fmt.Errorf("updater init: %w", err)
	}

	// The banner's action button is the ONLY thing that installs from a
	// notification. Anything that is not that action — a dismissal, a click on
	// the body — is deliberately a no-op, so a stray click cannot start a
	// migration.
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

	return func() {
		// On its own goroutine: this runs from an application event handler,
		// and the authorization prompt inside can block for minutes.
		go func() {
			activateNotifications(notify)
			log.Info().Str("feed", updateFeedURL).Str("version", cfg.CurrentVersion).
				Dur("first_check", initialCheckDelay).Dur("interval", updateCheckInterval).
				Msg("self-update enabled")
			pollForUpdates(ctx, wapp.Updater, notify, announce, seen, cfg.CurrentVersion,
				initialCheckDelay, updateCheckInterval)
		}()
	}, nil
}

// checkForUpdatesNow runs an on-demand check for the tray menu item, on a
// goroutine so the click returns immediately. Same path as the background
// poll: it reports, it does not install.
func checkForUpdatesNow(ctx context.Context, c updateChecker, n updateNotifier, a updateAnnouncer, seen notifyLog, current string) {
	go func() {
		if err := checkAndNotify(ctx, c, n, a, seen, current); err != nil {
			log.Warn().Err(err).Msg("manual update check failed")
		}
	}()
}

// updateMenuAction is what the tray's update item does. Once a check has found
// a release the item installs it; until then it runs a check.
//
// This is why the item is not merely informational. It is the only install
// path that survives a notification centre that never delivers — which on an
// unsigned .app is a real state, not a hypothetical one — and it is reached by
// the same deliberate click the banner's action button demands, so nothing
// here weakens the rule that an update never installs unprompted.
func updateMenuAction(
	ctx context.Context,
	p *pendingUpdate,
	c updateChecker,
	n updateNotifier,
	a updateAnnouncer,
	seen notifyLog,
	u updateInstaller,
	current string,
) {
	if rel := p.get(); rel != nil {
		log.Info().Str("version", rel.Version).Msg("installing update from the tray")
		go func() {
			if err := installAndRestart(ctx, u); err != nil {
				log.Warn().Err(err).Msg("update install failed")
			}
		}()
		return
	}
	checkForUpdatesNow(ctx, c, n, a, seen, current)
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
