//go:build desktop

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"github.com/wailsapp/wails/v3/pkg/updater"

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
}

func (f *fakeNotifier) RegisterNotificationCategory(c notifications.NotificationCategory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.categories = append(f.categories, c)
	return nil
}

func (f *fakeNotifier) SendNotificationWithActions(o notifications.NotificationOptions) error {
	f.mu.Lock()
	f.sent = append(f.sent, o)
	f.mu.Unlock()
	if f.delivered != nil {
		select {
		case f.delivered <- struct{}{}:
		default:
		}
	}
	return nil
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

	if err := checkAndNotify(t.Context(), c, n, "0.5.0"); err != nil {
		t.Fatalf("checkAndNotify: %v", err)
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on an up-to-date check, want 0: %+v", got, n.sent)
	}
}

func TestCheckAndNotifyPostsABannerWhenAnUpdateExists(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}}
	n := &fakeNotifier{}

	if err := checkAndNotify(t.Context(), c, n, "0.5.0"); err != nil {
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

	if err := checkAndNotify(t.Context(), c, n, "0.5.0"); err == nil {
		t.Error("checkAndNotify swallowed a check error")
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on a failed check, want 0", got)
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
	order      []string
	installErr error
	restartErr error
}

func (f *fakeInstaller) CheckAndInstall(context.Context) error {
	f.order = append(f.order, "install")
	return f.installErr
}

func (f *fakeInstaller) Restart(context.Context) error {
	f.order = append(f.order, "restart")
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

func TestPollForUpdatesChecksOnEveryTick(t *testing.T) {
	c := &fakeChecker{rel: &updater.Release{Version: "0.5.1"}, called: make(chan struct{}, 4)}
	n := &fakeNotifier{delivered: make(chan struct{}, 4)}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go pollForUpdates(ctx, c, n, "0.5.0", time.Millisecond)

	for i := range 2 {
		select {
		case <-n.delivered:
		case <-time.After(2 * time.Second):
			t.Fatalf("no notification after tick %d", i+1)
		}
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
		pollForUpdates(ctx, c, n, "0.5.0", time.Millisecond)
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

	checkForUpdatesNow(t.Context(), c, n, "0.5.0")

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

	checkForUpdatesNow(t.Context(), c, n, "0.5.0")

	select {
	case <-c.called:
	case <-time.After(2 * time.Second):
		t.Fatal("checkForUpdatesNow never reached Check")
	}
	if got := n.sentCount(); got != 0 {
		t.Errorf("sent %d notifications on a failed check, want 0", got)
	}
}
