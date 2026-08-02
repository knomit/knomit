//go:build desktop

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"github.com/wailsapp/wails/v3/pkg/updater"

	"knomit/internal/version"
	"knomit/tools/desktop/internal/paths"
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

// The real *updater.Updater must satisfy both roles. These assertions are what
// keep the two narrow interfaces honest as pkg/updater moves.
var (
	_ updateChecker   = (*updater.Updater)(nil)
	_ updateInstaller = (*updater.Updater)(nil)
)

// --- configuration guards ---------------------------------------------------

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
		})
	}
}

// The updater must never be able to open a window. pkg/updater's
// CheckAndInstall opens its window BEFORE it knows whether an update exists,
// and deliberately leaves it open on the up-to-date path — so any window mode
// but WindowNone turns every check into an uninvited panel the user must
// dismiss.
func TestUpdaterConfigNeverOpensAWindow(t *testing.T) {
	setBuild(t, "0.5.0", validTestKey)

	cfg, enabled, err := updaterConfig("darwin")
	if err != nil || !enabled {
		t.Fatalf("updaterConfig: enabled=%v err=%v", enabled, err)
	}
	if cfg.Window != updater.WindowNone {
		t.Errorf("Window = %#v, want updater.WindowNone — no window may ever appear", cfg.Window)
	}
	if _, isBuiltin := cfg.Window.(*updater.BuiltinWindow); isBuiltin {
		t.Error("Window is a BuiltinWindow; every check would pop a panel")
	}
}

// CheckInterval must stay ZERO. A non-zero value arms pkg/updater's
// periodicCheckLoop, which calls CheckAndInstall — and headless (WindowNone)
// that installs an update with no prompt at all, strictly worse than the
// window it replaced. Background polling is ours (pollForUpdates), and it only
// ever calls Check.
func TestUpdaterConfigLeavesPollingToUs(t *testing.T) {
	setBuild(t, "0.5.0", validTestKey)

	cfg, enabled, err := updaterConfig("darwin")
	if err != nil || !enabled {
		t.Fatalf("updaterConfig: enabled=%v err=%v", enabled, err)
	}
	if cfg.CheckInterval != 0 {
		t.Errorf("CheckInterval = %v, want 0 — non-zero auto-installs with no prompt", cfg.CheckInterval)
	}
	if updateCheckInterval <= 0 {
		t.Errorf("updateCheckInterval = %v, want a positive poll cadence", updateCheckInterval)
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

// --- fakes ------------------------------------------------------------------

type fakeChecker struct {
	mu     sync.Mutex
	rel    *updater.Release
	err    error
	calls  int
	called chan struct{}
}

func (f *fakeChecker) Check(context.Context) (*updater.Release, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	return f.rel, f.err
}

func (f *fakeChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeNotifier struct {
	mu         sync.Mutex
	categories []notifications.NotificationCategory
	sent       []notifications.NotificationOptions
	delivered  chan struct{}

	// Authorization outcome, for the activateNotifications tests. The zero
	// value (denied, no error) is the state an unsigned bundle can land in.
	granted     bool
	authErr     error
	authCalls   int
	sendErr     error
	registerErr error
}

func (f *fakeNotifier) RequestNotificationAuthorization() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	return f.granted, f.authErr
}

func (f *fakeNotifier) RegisterNotificationCategory(c notifications.NotificationCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.categories = append(f.categories, c)
	return f.registerErr
}

func (f *fakeNotifier) SendNotificationWithActions(o notifications.NotificationOptions) error {
	f.mu.Lock()
	f.sent = append(f.sent, o)
	sendErr := f.sendErr
	f.mu.Unlock()
	if f.delivered != nil {
		select {
		case f.delivered <- struct{}{}:
		default:
		}
	}
	return sendErr
}

func (f *fakeNotifier) categoryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.categories)
}

// fakeAnnouncer stands in for the tray surfaces (badge + menu item).
type fakeAnnouncer struct {
	mu       sync.Mutex
	released []*updater.Release
}

func (f *fakeAnnouncer) AnnounceUpdate(rel *updater.Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, rel)
}

// fakeNotifyLog is an in-memory notifyLog. saveErr simulates a state file that
// cannot be written.
type fakeNotifyLog struct {
	mu      sync.Mutex
	last    string
	saveErr error
	marks   int
}

func (f *fakeNotifyLog) AlreadyNotified(version string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return version != "" && version == f.last
}

func (f *fakeNotifyLog) MarkNotified(version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.last = version
	return nil
}

func (f *fakeAnnouncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.released)
}

func (f *fakeAnnouncer) last() *updater.Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.released) == 0 {
		return nil
	}
	return f.released[len(f.released)-1]
}

func (f *fakeNotifier) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeNotifier) lastSent() notifications.NotificationOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return notifications.NotificationOptions{}
	}
	return f.sent[len(f.sent)-1]
}

// --- notification behaviour -------------------------------------------------

// A check that finds nothing must be SILENT. The banner is the only update UI
// there is, so notifying on every up-to-date poll would reproduce, in the
// notification centre, the exact recurring interruption the windowless design
// exists to remove.
func TestCheckAndNotifyStaysSilentWhenCurrent(t *testing.T) {
	c := &fakeChecker{rel: nil}
	n := &fakeNotifier{}
	a := &fakeAnnouncer{}

	if err := checkAndNotify(t.Context(), c, n, a, &fakeNotifyLog{}, "0.5.0"); err != nil {
		t.Fatalf("checkAndNotify: %v", err)
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on an up-to-date check, want 0: %+v", got, n.sent)
	}
	// Silent means silent on BOTH surfaces: no badge either.
	if got := a.count(); got != 0 {
		t.Errorf("announced %d releases on an up-to-date check, want 0", got)
	}
}

func TestCheckAndNotifyPostsABannerWhenAnUpdateExists(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{}
	a := &fakeAnnouncer{}

	if err := checkAndNotify(t.Context(), c, n, a, &fakeNotifyLog{}, "0.5.0"); err != nil {
		t.Fatalf("checkAndNotify: %v", err)
	}
	if got := n.sentCount(); got != 1 {
		t.Fatalf("sent %d notifications, want 1", got)
	}
	sent := n.lastSent()
	if !strings.Contains(sent.Title, "0.5.1") {
		t.Errorf("Title = %q, want the available version in it", sent.Title)
	}
	if !strings.Contains(sent.Subtitle, "0.5.0") {
		t.Errorf("Subtitle = %q, want the current version in it", sent.Subtitle)
	}
	// The banner is the consent surface for an irreversible migration, so it
	// has to say so rather than merely announce availability.
	if !strings.Contains(sent.Body, "not reverse") {
		t.Errorf("Body = %q, want the irreversible-migration warning", sent.Body)
	}
	// Without a matching category the banner delivers with NO buttons — and
	// the action button is the only thing that can install.
	if sent.CategoryID != updateCategoryID {
		t.Errorf("CategoryID = %q, want %q, else the banner has no Install button", sent.CategoryID, updateCategoryID)
	}
	if sent.ID == "" {
		t.Error("notification has no ID; SendNotificationWithActions rejects it")
	}
}

// A failed check must surface the error rather than pass for "up to date".
func TestCheckAndNotifyPropagatesCheckErrors(t *testing.T) {
	c := &fakeChecker{err: errors.New("feed unreachable")}
	n := &fakeNotifier{}
	a := &fakeAnnouncer{}

	if err := checkAndNotify(t.Context(), c, n, a, &fakeNotifyLog{}, "0.5.0"); err == nil {
		t.Error("checkAndNotify swallowed a check error")
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on a failed check, want 0", got)
	}
	if got := a.count(); got != 0 {
		t.Errorf("announced %d releases on a failed check, want 0", got)
	}
}

// The tray badge must NOT be contingent on the notification centre. knomit's
// .app is unsigned and pkg/services/notifications documents "packaged and
// signed" as its requirement, so a rejected banner is a state real users will
// be in — and it is exactly the state where the badge is the only thing left
// telling them an update exists.
func TestCheckAndNotifyAnnouncesEvenWhenTheBannerFails(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{sendErr: errors.New("notifications not permitted")}
	a := &fakeAnnouncer{}

	if err := checkAndNotify(t.Context(), c, n, a, &fakeNotifyLog{}, "0.5.0"); err == nil {
		t.Error("checkAndNotify hid a failed notification; the log would never show it")
	}
	if got := a.count(); got != 1 {
		t.Fatalf("announced %d releases, want 1 — the badge must survive a dead notification centre", got)
	}
	if got := a.last(); got == nil || got.Version != "0.5.1" {
		t.Errorf("announced %+v, want the 0.5.1 release", got)
	}
}

func TestRegisterUpdateCategoryDeclaresTheInstallAction(t *testing.T) {
	n := &fakeNotifier{}
	if err := registerUpdateCategory(n); err != nil {
		t.Fatalf("registerUpdateCategory: %v", err)
	}
	if len(n.categories) != 1 {
		t.Fatalf("registered %d categories, want 1", len(n.categories))
	}
	cat := n.categories[0]
	// The banner names this same ID; if the two drift the button silently
	// never appears and there is no way left to install.
	if cat.ID != updateCategoryID {
		t.Errorf("category ID = %q, want %q", cat.ID, updateCategoryID)
	}
	if len(cat.Actions) != 1 || cat.Actions[0].ID != updateInstallActionID {
		t.Fatalf("actions = %+v, want exactly the %q action", cat.Actions, updateInstallActionID)
	}
	if cat.Actions[0].Title == "" {
		t.Error("the install action has no title; the button would render blank")
	}
}

// --- install path -----------------------------------------------------------

type fakeInstaller struct {
	mu         sync.Mutex
	order      []string
	installErr error
	restartErr error
	// done is signalled after the last call an install makes, so a test can
	// wait on updateMenuAction's goroutine rather than sleep.
	done chan struct{}
}

func (f *fakeInstaller) record(call string) {
	f.mu.Lock()
	f.order = append(f.order, call)
	f.mu.Unlock()
}

func (f *fakeInstaller) signal() {
	if f.done == nil {
		return
	}
	select {
	case f.done <- struct{}{}:
	default:
	}
}

func (f *fakeInstaller) CheckAndInstall(context.Context) error {
	f.record("install")
	if f.installErr != nil {
		f.signal() // nothing follows a failed install
	}
	return f.installErr
}

func (f *fakeInstaller) Restart(context.Context) error {
	f.record("restart")
	f.signal()
	return f.restartErr
}

// Install must precede restart, and both must run on the happy path: an update
// that installs but never relaunches leaves the user on the old version with
// no sign anything happened.
func TestInstallAndRestartInstallsThenRestarts(t *testing.T) {
	f := &fakeInstaller{}
	if err := installAndRestart(t.Context(), f); err != nil {
		t.Fatalf("installAndRestart: %v", err)
	}
	if len(f.order) != 2 || f.order[0] != "install" || f.order[1] != "restart" {
		t.Errorf("call order = %v, want [install restart]", f.order)
	}
}

// A failed install must NOT restart. Restart quits the app to hand over to the
// swap helper, so restarting with nothing staged is indistinguishable from a
// crash.
func TestInstallAndRestartDoesNotRestartAfterAFailedInstall(t *testing.T) {
	f := &fakeInstaller{installErr: errors.New("verification failed")}

	if err := installAndRestart(t.Context(), f); err == nil {
		t.Error("installAndRestart returned nil after a failed install")
	}
	for _, call := range f.order {
		if call == "restart" {
			t.Fatal("restarted after a failed install — the app would quit having installed nothing")
		}
	}
}

// --- polling ----------------------------------------------------------------

// Every tick must reach Check — asserted on the CHECK, not on a notification,
// because the banner now fires only once per version and would never arrive a
// second time for the same release.
func TestPollForUpdatesChecksOnEveryTick(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}, called: make(chan struct{}, 4)}
	n := &fakeNotifier{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go pollForUpdates(ctx, c, n, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0", time.Millisecond, time.Millisecond)

	for i := range 3 {
		select {
		case <-c.called:
		case <-time.After(2 * time.Second):
			t.Fatalf("no check after tick %d", i+1)
		}
	}
}

// The first check must NOT wait for the poll interval. The tray badge is a
// passive surface, so a ticker-only loop leaves a freshly launched app showing
// nothing for six hours — and a session shorter than that learns nothing at
// all.
func TestPollForUpdatesChecksShortlyAfterStartup(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}, called: make(chan struct{}, 1)}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// An interval far longer than the test could wait for: only the initial
	// check can satisfy this.
	go pollForUpdates(ctx, c, &fakeNotifier{}, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0", time.Millisecond, time.Hour)

	select {
	case <-c.called:
	case <-time.After(2 * time.Second):
		t.Fatal("no check before the first interval elapsed; the badge would stay hidden for a full period")
	}
}

// A context cancelled before the initial delay elapses must stop the loop
// without checking — otherwise a fast quit still fires a network request on
// behalf of a process that is going away.
func TestPollForUpdatesHonoursCancellationDuringTheInitialDelay(t *testing.T) {
	c := &fakeChecker{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		pollForUpdates(ctx, c, &fakeNotifier{}, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0", time.Hour, time.Hour)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollForUpdates waited out the initial delay on a cancelled context")
	}
	if got := c.callCount(); got != 0 {
		t.Errorf("Check called %d times on a cancelled context, want 0", got)
	}
}

// The loop must exit with the app's context, or it outlives shutdown and keeps
// polling a feed on behalf of a process that is going away.
func TestPollForUpdatesStopsWhenContextIsCancelled(t *testing.T) {
	c := &fakeChecker{called: make(chan struct{}, 1)}
	n := &fakeNotifier{}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		pollForUpdates(ctx, c, n, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0", time.Millisecond, time.Millisecond)
		close(done)
	}()

	<-c.called // the loop is running
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollForUpdates ignored context cancellation")
	}
}

// The tray item takes the SAME reporting path as the background poll: it is
// handed an updateChecker, which has no method that can install. A manual
// check that finds an update notifies; one that finds nothing is silent.
func TestCheckForUpdatesNowReportsWithoutInstalling(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{delivered: make(chan struct{}, 1)}

	checkForUpdatesNow(t.Context(), c, n, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0")

	select {
	case <-n.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("checkForUpdatesNow never posted a banner")
	}
	if got := c.callCount(); got != 1 {
		t.Errorf("Check called %d times, want 1", got)
	}
}

// A failing manual check must be logged and swallowed, never panic the app.
func TestCheckForUpdatesNowSwallowsErrors(t *testing.T) {
	c := &fakeChecker{err: errors.New("feed unreachable"), called: make(chan struct{}, 1)}
	n := &fakeNotifier{}

	checkForUpdatesNow(t.Context(), c, n, &fakeAnnouncer{}, &fakeNotifyLog{}, "0.5.0")

	select {
	case <-c.called:
	case <-time.After(2 * time.Second):
		t.Fatal("checkForUpdatesNow never reached Check")
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on a failed check, want 0", got)
	}
}

// --- notification activation (deferred to ApplicationStarted) ---------------

// The category must be registered EVEN when authorization was refused.
// Permission can be granted later from System Settings without restarting
// Knomit, and a banner naming an unregistered category still delivers — it
// just silently loses its buttons, which is the one failure a user cannot
// diagnose.
func TestActivateNotificationsRegistersTheCategoryEvenWhenDenied(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    *fakeNotifier
	}{
		{"granted", &fakeNotifier{granted: true}},
		{"denied", &fakeNotifier{granted: false}},
		{"authorization errored", &fakeNotifier{authErr: errors.New("not available")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			activateNotifications(tt.n)

			if tt.n.authCalls != 1 {
				t.Errorf("RequestNotificationAuthorization called %d times, want 1", tt.n.authCalls)
			}
			if got := tt.n.categoryCount(); got != 1 {
				t.Errorf("registered %d categories, want 1 — a banner with no category loses its Install button", got)
			}
		})
	}
}

// Both steps are best-effort: a notification centre that refuses everything
// must leave a running updater and a working tray, not an aborted startup.
func TestActivateNotificationsSwallowsEveryFailure(t *testing.T) {
	n := &fakeNotifier{
		authErr:     errors.New("no bundle identifier"),
		registerErr: errors.New("delegate lost"),
	}
	activateNotifications(n) // must not panic
	if n.authCalls != 1 {
		t.Errorf("authCalls = %d, want 1", n.authCalls)
	}
}

// --- the tray as an install path --------------------------------------------

// A later check finding nothing must NOT retract a release already announced.
// Check returns nil both for "you are current" and for a feed that is briefly
// empty, misconfigured or unreachable — and clearing on the second case would
// turn a transient feed problem into an update that silently vanished from the
// tray.
func TestPendingUpdateIsNeverCleared(t *testing.T) {
	p := &pendingUpdate{}
	if p.get() != nil {
		t.Fatal("a fresh pendingUpdate reports a release")
	}

	p.set(&updater.Release{Version: "0.5.1"})
	p.set(nil) // a later check found nothing

	got := p.get()
	if got == nil || got.Version != "0.5.1" {
		t.Errorf("pending = %+v, want the 0.5.1 release to survive a nil check", got)
	}
}

// set reports newness by VERSION, not pointer identity. Every poll builds a
// fresh *updater.Release, so a pointer comparison would call every check new
// and re-banner the same version forever.
func TestPendingUpdateReportsNewVersionsOnly(t *testing.T) {
	p := &pendingUpdate{}

	if !p.set(&updater.Release{Version: "0.5.1"}) {
		t.Error("the first sighting of 0.5.1 was not reported as new")
	}
	// A distinct pointer carrying the same version — what the next poll produces.
	if p.set(&updater.Release{Version: "0.5.1"}) {
		t.Error("a re-poll of 0.5.1 was reported as new; the banner would repeat every interval")
	}
	if !p.set(&updater.Release{Version: "0.5.2"}) {
		t.Error("0.5.2 was not reported as new; a genuinely newer release must announce")
	}
	if p.set(nil) {
		t.Error("a nil release was reported as new")
	}
}

// The banner is a one-time EVENT, the tray a persistent STATE. Every check
// keeps refreshing the tray; only a version the user has not been told about
// reaches the notification centre. Without this an unclaimed update re-banners
// every interval forever — the recurring interruption the windowless design
// exists to remove, merely relocated to Notification Centre.
func TestCheckAndNotifyBannersOncePerVersion(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{}
	a := &fakeAnnouncer{}
	// ONE log across every check — it is the thing that carries the memory.
	seen := &fakeNotifyLog{}

	for range 3 {
		if err := checkAndNotify(t.Context(), c, n, a, seen, "0.5.0"); err != nil {
			t.Fatalf("checkAndNotify: %v", err)
		}
	}
	if got := n.sentCount(); got != 1 {
		t.Errorf("sent %d banners for one version across 3 checks, want 1", got)
	}
	// The tray is still told every time — it is the surface that must stay
	// accurate, and it decides for itself what is worth redrawing.
	if got := a.count(); got != 3 {
		t.Errorf("announced %d times across 3 checks, want 3", got)
	}

	// A genuinely newer release is new information and banners again.
	c.rel = &updater.Release{Version: "0.5.2"}
	if err := checkAndNotify(t.Context(), c, n, a, seen, "0.5.0"); err != nil {
		t.Fatalf("checkAndNotify: %v", err)
	}
	if got := n.sentCount(); got != 2 {
		t.Errorf("sent %d banners after a newer release appeared, want 2", got)
	}
	if got := n.lastSent(); !strings.Contains(got.Title, "0.5.2") {
		t.Errorf("Title = %q, want the newer version", got.Title)
	}
}

// The memory is what a RESTART reads back. A fresh process with a log that
// already names the version must badge the tray and stay silent — without
// this, "once per version" is really "once per version per launch".
func TestCheckAndNotifyStaysSilentForAVersionAnnouncedInAnEarlierRun(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{}
	a := &fakeAnnouncer{}
	// Everything else is fresh, as it would be after a relaunch.
	seen := &fakeNotifyLog{last: "0.5.1"}

	if err := checkAndNotify(t.Context(), c, n, a, seen, "0.5.0"); err != nil {
		t.Fatalf("checkAndNotify: %v", err)
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d banners for a version announced in an earlier run, want 0", got)
	}
	// The update IS still pending, so the tray must still show it.
	if got := a.count(); got != 1 {
		t.Errorf("announced %d times, want 1 — the badge must return after a restart", got)
	}
}

// A banner that was never delivered must NOT be recorded. The send reached
// nobody, so burning the one announcement that version gets would leave a user
// who grants notification permission afterwards with no banner at all.
func TestCheckAndNotifyDoesNotRecordAnUndeliveredBanner(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{sendErr: errors.New("notifications not permitted")}
	seen := &fakeNotifyLog{}

	if err := checkAndNotify(t.Context(), c, n, &fakeAnnouncer{}, seen, "0.5.0"); err == nil {
		t.Error("checkAndNotify hid a failed notification")
	}
	if seen.marks != 0 {
		t.Errorf("MarkNotified called %d times after a failed send, want 0", seen.marks)
	}
	if seen.AlreadyNotified("0.5.1") {
		t.Error("0.5.1 recorded as announced despite the banner never being delivered")
	}

	// Permission granted afterwards: the retry now lands, and is recorded.
	n.mu.Lock()
	n.sendErr = nil
	n.mu.Unlock()
	if err := checkAndNotify(t.Context(), c, n, &fakeAnnouncer{}, seen, "0.5.0"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !seen.AlreadyNotified("0.5.1") {
		t.Error("a delivered banner was not recorded")
	}
}

// A state file that cannot be written must not break the check. The banner was
// delivered — the user has been told — and the only consequence is that the
// next launch tells them again.
func TestCheckAndNotifySurvivesAnUnwritableStateFile(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{}
	seen := &fakeNotifyLog{saveErr: errors.New("read-only filesystem")}

	if err := checkAndNotify(t.Context(), c, n, &fakeAnnouncer{}, seen, "0.5.0"); err != nil {
		t.Errorf("checkAndNotify = %v, want nil — a delivered banner is a success", err)
	}
	if got := n.sentCount(); got != 1 {
		t.Errorf("sent %d banners, want 1", got)
	}
}

// --- the persistent notify log ----------------------------------------------

// The real log against a real file, across two constructions — which is what a
// quit and relaunch actually is.
func TestPersistentNotifyLogSurvivesReconstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")

	first := &persistentNotifyLog{path: path}
	if first.AlreadyNotified("0.5.1") {
		t.Error("a fresh log claims 0.5.1 was already announced")
	}
	if err := first.MarkNotified("0.5.1"); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}

	// A new process reading the same file — through the real load path, so a
	// log that forgot to read the file back would fail here.
	second := loadNotifyLog(path)
	if !second.AlreadyNotified("0.5.1") {
		t.Error("0.5.1 was not remembered across reconstruction; the banner would repeat every launch")
	}
	if second.AlreadyNotified("0.5.2") {
		t.Error("a newer version was treated as already announced")
	}
}

// An empty path is the degraded mode newNotifyLog falls into when the state
// dir cannot be resolved. It must behave like the old session-scoped memory —
// remembering within the run, writing nothing — rather than erroring on every
// check.
func TestPersistentNotifyLogWithNoPathDegradesToInMemory(t *testing.T) {
	l := &persistentNotifyLog{}

	if err := l.MarkNotified("0.5.1"); err != nil {
		t.Errorf("MarkNotified with no path = %v, want nil", err)
	}
	if l.AlreadyNotified("0.5.1") {
		t.Error("a pathless log recorded the version; it has nowhere to put it")
	}
}

// The 6-hourly poll re-announces the same version forever. Rewriting an
// identical file every interval is pure waste, so a repeat must not touch disk.
func TestPersistentNotifyLogDoesNotRewriteAnUnchangedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.json")
	l := &persistentNotifyLog{path: path}

	if err := l.MarkNotified("0.5.1"); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := l.MarkNotified("0.5.1"); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A rewrite renames a fresh temp file over the destination, changing the
	// inode — os.SameFile is what detects that.
	if !os.SameFile(first, second) {
		t.Error("MarkNotified rewrote the state file for an unchanged version")
	}
}

// The path must be the one paths.UpdateStatePath resolves, beside the server
// lockfile. A log pointed somewhere else silently remembers nothing.
func TestNewNotifyLogUsesTheStateDir(t *testing.T) {
	want, err := paths.UpdateStatePath()
	if err != nil {
		t.Skipf("no state dir on this host: %v", err)
	}
	if got := newNotifyLog().path; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// Until a release is known the tray item is a check button.
func TestUpdateMenuActionChecksWhenNothingIsPending(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{delivered: make(chan struct{}, 1)}
	a := &fakeAnnouncer{}
	inst := &fakeInstaller{}

	updateMenuAction(t.Context(), &pendingUpdate{}, c, n, a, &fakeNotifyLog{}, inst, "0.5.0")

	select {
	case <-n.delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the tray item never ran a check")
	}
	if len(inst.order) != 0 {
		t.Errorf("the tray item installed %v without a release having been found", inst.order)
	}
}

// Once a release IS known the same item installs it. This is the only install
// path that survives a notification centre that never delivers — which on an
// unsigned .app is a real state, not a hypothetical one.
func TestUpdateMenuActionInstallsWhatTheCheckFound(t *testing.T) {
	p := &pendingUpdate{}
	p.set(&updater.Release{Version: "0.5.1"})
	c := &fakeChecker{}
	inst := &fakeInstaller{done: make(chan struct{}, 1)}

	updateMenuAction(t.Context(), p, c, &fakeNotifier{}, &fakeAnnouncer{}, &fakeNotifyLog{}, inst, "0.5.0")

	select {
	case <-inst.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the tray item never installed the release it had already found")
	}
	if got := c.callCount(); got != 0 {
		t.Errorf("Check called %d times, want 0 — the release was already known", got)
	}
	if len(inst.order) == 0 || inst.order[0] != "install" {
		t.Errorf("call order = %v, want install first", inst.order)
	}
}
