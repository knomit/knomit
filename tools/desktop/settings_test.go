//go:build desktop

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

// Config layers defaults -> knomit.toml -> env, so an exported KNOMIT_PORT
// BEATS anything the dialog writes. Without surfacing that, Save appears to
// work and silently changes nothing — the worst possible failure for a
// settings form.
func TestEnvOverridesNamesTheVariablesThatWin(t *testing.T) {
	env := map[string]string{
		"KNOMIT_PORT":       "30000",
		"KNOMIT_LOG_FORMAT": "json",
	}
	got := envOverrides(func(k string) string { return env[k] })

	for _, want := range []string{"KNOMIT_PORT", "KNOMIT_LOG_FORMAT"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s not reported as overridden; got %v", want, got)
		}
	}
	if slices.Contains(got, "KNOMIT_LOG_LEVEL") {
		t.Errorf("unset variable reported as overridden: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly the two set variables, got %v", got)
	}
}

// The other half of the same behaviour: with nothing exported the form must be
// told the file is authoritative, not handed a phantom override.
func TestEnvOverridesReportsNothingWhenNothingIsSet(t *testing.T) {
	if got := envOverrides(func(string) string { return "" }); len(got) != 0 {
		t.Errorf("expected no overrides, got %v", got)
	}
}

// Every editable field must have an override entry: a field that is silently
// missing from envKeys is a field the form will claim it can change when it
// cannot.
func TestEnvOverridesCoversEveryEditableField(t *testing.T) {
	all := envOverrides(func(string) string { return "set" })
	for _, want := range []string{"KNOMIT_PORT", "KNOMIT_LOG_LEVEL", "KNOMIT_LOG_FORMAT"} {
		if !slices.Contains(all, want) {
			t.Errorf("%s is not tracked as an override at all; got %v", want, all)
		}
	}
}

func TestValidateSettingsRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Settings
	}{
		{"port not a number", Settings{Port: "http", LogLevel: "info", LogFormat: "console"}},
		{"port privileged", Settings{Port: "80", LogLevel: "info", LogFormat: "console"}},
		{"port out of range", Settings{Port: "70000", LogLevel: "info", LogFormat: "console"}},
		{"port empty", Settings{Port: "", LogLevel: "info", LogFormat: "console"}},
		{"port zero", Settings{Port: "0", LogLevel: "info", LogFormat: "console"}},
		{"unknown level", Settings{Port: "19278", LogLevel: "chatty", LogFormat: "console"}},
		// Nothing downstream catches a blanked level: zerolog.ParseLevel("")
		// succeeds (NoLevel, nil) and config.Validate skips an empty level
		// outright, so a bare ParseLevel check would let the dialog persist one
		// and silently drop the user's choice back to logging.Build's "info".
		{"empty level", Settings{Port: "19278", LogLevel: "", LogFormat: "console"}},
		{"unknown format", Settings{Port: "19278", LogLevel: "info", LogFormat: "xml"}},
		{"empty format", Settings{Port: "19278", LogLevel: "info", LogFormat: ""}},
	} {
		if err := validateSettings(tc.s); err == nil {
			t.Errorf("%s: accepted an invalid setting %+v", tc.name, tc.s)
		}
	}
}

func TestValidateSettingsAcceptsAValidSet(t *testing.T) {
	s := Settings{Port: "20000", LogLevel: "debug", LogFormat: "console"}
	if err := validateSettings(s); err != nil {
		t.Errorf("rejected a valid set: %v", err)
	}
	// Everything the form can offer must be accepted, or the dialog gets to
	// show a choice that Save then refuses.
	for _, lvl := range []string{"trace", "debug", "info", "warn", "error"} {
		for _, format := range []string{"console", "json"} {
			s := Settings{Port: "1024", LogLevel: lvl, LogFormat: format}
			if err := validateSettings(s); err != nil {
				t.Errorf("rejected %+v: %v", s, err)
			}
		}
	}
	// The boundaries themselves are legal.
	for _, port := range []string{"1024", "65535"} {
		if err := validateSettings(Settings{Port: port, LogLevel: "info", LogFormat: "json"}); err != nil {
			t.Errorf("rejected boundary port %s: %v", port, err)
		}
	}
}

// stubToggler stands in for the OS autostart registration. It records every
// call, so a test can assert not just the final state — which a toggler that
// did nothing at all would also satisfy — but exactly which transitions ran.
type stubToggler struct {
	on        bool
	calls     []string
	enableErr error
}

func (s *stubToggler) Enabled() (bool, error) { return s.on, nil }

func (s *stubToggler) Enable() error {
	s.calls = append(s.calls, "enable")
	if s.enableErr != nil {
		return s.enableErr
	}
	s.on = true
	return nil
}

func (s *stubToggler) Disable() error {
	s.calls = append(s.calls, "disable")
	s.on = false
	return nil
}

// Two backends behind one form: three keys go to knomit.toml, start-at-login
// goes to the OS. A regression that sent autostart to the TOML would write a
// key knomit ignores and leave the login item untouched.
func TestApplySettingsWritesTOMLAndTogglesAutostart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	if err := os.WriteFile(path, []byte("# hand written\n[log]\nlevel = \"info\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tog := &stubToggler{}

	s := Settings{Port: "20000", LogLevel: "debug", LogFormat: "console", StartAtLogin: true}
	if err := applySettings(s, path, tog, nil); err != nil {
		t.Fatalf("applySettings: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`port = "20000"`, `level = "debug"`, `format = "console"`, "# hand written"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in written config:\n%s", want, got)
		}
	}
	// Updated in place, not appended alongside the old one: two `level =` lines
	// in one table is a knomit.toml that no longer parses.
	if n := strings.Count(got, "level ="); n != 1 {
		t.Errorf("level assigned %d times, want 1:\n%s", n, got)
	}
	if strings.Contains(got, `level = "info"`) {
		t.Errorf("old level survived the edit:\n%s", got)
	}
	if strings.Contains(got, "start_at_login") {
		t.Errorf("autostart leaked into knomit.toml:\n%s", got)
	}
	if !tog.on {
		t.Error("start at login was not enabled through the OS toggler")
	}
}

// The mirror case. A save that can only ever turn autostart ON is a checkbox
// the user cannot uncheck.
func TestApplySettingsDisablesAutostartWhenUnchecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	tog := &stubToggler{on: true}

	s := Settings{Port: "20000", LogLevel: "info", LogFormat: "console", StartAtLogin: false}
	if err := applySettings(s, path, tog, nil); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	if tog.on {
		t.Error("start at login was not disabled through the OS toggler")
	}
}

// Saving with the checkbox unchanged must not re-register the login item — on
// macOS that rewrites a LaunchAgent plist for no reason.
func TestApplySettingsLeavesAutostartAloneWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	tog := &stubToggler{on: true}

	s := Settings{Port: "20000", LogLevel: "info", LogFormat: "console", StartAtLogin: true}
	if err := applySettings(s, path, tog, nil); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	if len(tog.calls) != 0 {
		t.Errorf("toggler was called %v for an unchanged setting", tog.calls)
	}
}

// The form shows the value the ENVIRONMENT supplied for an overridden field
// (GetSettings reads after the env layer) and disables it, saying "saving
// cannot change this". Writing that value back would make the message a lie
// and persist a port the user never typed — one they cannot see in the
// disabled field and will not discover until they unset the variable and find
// knomit still on it. The file's own value must survive the save untouched.
func TestApplySettingsDoesNotWriteAnEnvOverriddenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	if err := os.WriteFile(path, []byte("port = \"19278\"\n[log]\nlevel = \"info\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// What the form sends with KNOMIT_PORT=30000 exported: the port field
	// carries the environment's value, and the user edited only the level.
	s := Settings{Port: "30000", LogLevel: "debug", LogFormat: "json"}
	if err := applySettings(s, path, &stubToggler{}, []string{"KNOMIT_PORT"}); err != nil {
		t.Fatalf("applySettings: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `port = "19278"`) {
		t.Errorf("the file's own port did not survive the save:\n%s", got)
	}
	if strings.Contains(got, "30000") {
		t.Errorf("the environment's port was written to the file:\n%s", got)
	}
	// The keys the environment does NOT own must still be written, or the
	// override turns into a read-only dialog.
	for _, want := range []string{`level = "debug"`, `format = "json"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q; a single override froze the whole file:\n%s", want, got)
		}
	}
}

// The nastier half: a fresh install has no knomit.toml at all, so the
// overridden key is not merely left alone — it must never be CREATED. A writer
// that only skipped the update path would silently seed the file with the
// environment's value on the very first save.
func TestApplySettingsDoesNotCreateAnEnvOverriddenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")

	s := Settings{Port: "30000", LogLevel: "debug", LogFormat: "console"}
	if err := applySettings(s, path, &stubToggler{}, []string{"KNOMIT_PORT"}); err != nil {
		t.Fatalf("applySettings: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "port") {
		t.Errorf("a port key was created for an env-owned setting:\n%s", got)
	}
	if !strings.Contains(got, `level = "debug"`) {
		t.Errorf("the editable keys were not written:\n%s", got)
	}
}

// Every overridden key at once: the file must come back byte-identical apart
// from nothing at all. Guards the case where the skip is wired for one key and
// not the others.
func TestApplySettingsSkipsEveryOverriddenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	original := "port = \"19278\"\n[log]\nlevel = \"info\"\nformat = \"console\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	s := Settings{Port: "30000", LogLevel: "error", LogFormat: "json"}
	if err := applySettings(s, path, &stubToggler{}, envKeys); err != nil {
		t.Fatalf("applySettings: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Errorf("the file changed although every key was env-owned:\ngot:\n%s\nwant:\n%s", b, original)
	}
}

// The dialog must never be able to write a file that stops the next launch.
func TestApplySettingsRejectsInvalidBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	original := "[log]\nlevel = \"info\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	tog := &stubToggler{}

	bad := Settings{Port: "not-a-port", LogLevel: "info", LogFormat: "console", StartAtLogin: true}
	if err := applySettings(bad, path, tog, nil); err == nil {
		t.Fatal("applySettings accepted an invalid port")
	}

	b, _ := os.ReadFile(path)
	if string(b) != original {
		t.Errorf("config was modified despite validation failing:\n%s", b)
	}
	// A rejected save must be a no-op on BOTH backends, not just the file.
	if len(tog.calls) != 0 || tog.on {
		t.Errorf("autostart was touched by a rejected save (calls=%v on=%v)", tog.calls, tog.on)
	}
}

// knomit.toml need not exist — a fresh install has none.
func TestApplySettingsCreatesAMissingConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	s := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}

	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not created: %v", err)
	}
	if !strings.Contains(string(b), `port = "19278"`) {
		t.Errorf("created config missing the port:\n%s", b)
	}
	// The file knomit reads on the next launch has to be valid TOML with the
	// three keys in the right tables — Contains alone would pass on garbage.
	assertConfigRoundTrips(t, path, "19278", "info", "console")
}

// The two halves must not be able to disagree. If the OS refuses to register
// the login item, the file must not already say the new port — the user would
// be told the save failed while half of it silently stuck.
func TestApplySettingsLeavesTheFileAloneWhenAutostartFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	original := "port = \"19278\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("login item refused")
	tog := &stubToggler{enableErr: boom}

	s := Settings{Port: "20000", LogLevel: "info", LogFormat: "console", StartAtLogin: true}
	err := applySettings(s, path, tog, nil)
	if err == nil {
		t.Fatal("applySettings hid an autostart failure")
	}
	if !errors.Is(err, boom) {
		t.Errorf("autostart failure not surfaced: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != original {
		t.Errorf("config was written despite the autostart failure:\n%s", b)
	}
}

// And the other direction: if the file cannot be written, the login item must
// not be left registered against settings that were never saved.
func TestApplySettingsRollsBackAutostartWhenTheWriteFails(t *testing.T) {
	// A path whose parent directory does not exist: every write to it fails.
	path := filepath.Join(t.TempDir(), "no-such-dir", "knomit.toml")
	tog := &stubToggler{on: false}

	s := Settings{Port: "20000", LogLevel: "info", LogFormat: "console", StartAtLogin: true}
	if err := applySettings(s, path, tog, nil); err == nil {
		t.Fatal("applySettings reported success with an unwritable config path")
	}
	if tog.on {
		t.Error("autostart was left enabled after the config write failed")
	}
	// Final state alone would also be satisfied by a toggle that never ran.
	// Both calls must be there, in order: registered, then rolled back.
	if !slices.Equal(tog.calls, []string{"enable", "disable"}) {
		t.Errorf("toggler calls = %v, want [enable disable] (enable then rollback)", tog.calls)
	}
}

// knomit.toml can hold an API key ([llm] api_key). A dialog-created file must
// not be world-readable, and an existing file's mode is the user's business.
func TestApplySettingsFilePermissions(t *testing.T) {
	dir := t.TempDir()
	s := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}

	fresh := filepath.Join(dir, "fresh.toml")
	if err := applySettings(s, fresh, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("new config is group/world readable: %v", fi.Mode().Perm())
	}

	existing := filepath.Join(dir, "existing.toml")
	if err := os.WriteFile(existing, []byte("port = \"19278\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applySettings(s, existing, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("existing config mode changed to %v, want 0644", fi.Mode().Perm())
	}
}

// The effective port arrives from the boot goroutine while the UI thread can
// already be calling GetSettings. Run under -race, this is the guard on that.
func TestSetEffectivePortIsRaceFree(t *testing.T) {
	n := newNativeService(filepath.Join(t.TempDir(), "knomit.toml"), "", &stubToggler{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n.setEffectivePort(41234)
	}()
	go func() {
		defer wg.Done()
		_ = n.currentEffectivePort()
	}()
	wg.Wait()

	if got := n.currentEffectivePort(); got != 41234 {
		t.Errorf("effective port = %d, want 41234", got)
	}
}

func TestPortFromBaseURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"http://127.0.0.1:19278", 19278},
		{"http://127.0.0.1:54321/", 54321},
		{"", 0},
		{"http://127.0.0.1", 0},
		{"::not a url", 0},
	} {
		if got := portFromBaseURL(tc.in); got != tc.want {
			t.Errorf("portFromBaseURL(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// desktopHome points config.Load at a temp dir and returns it, so a test can
// exercise GetSettings without reading the developer's real knomit.toml.
func desktopHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	// Any of these left set in the developer's shell would otherwise leak into
	// the assertions below.
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	// SaveSettings rebuilds the global logger; put it back for the rest of the
	// package.
	prev, prevLevel := log.Logger, zerolog.GlobalLevel()
	t.Cleanup(func() {
		log.Logger = prev
		zerolog.SetGlobalLevel(prevLevel)
	})
	return home
}

// The round trip the dialog actually performs: Save, then reopen and read back.
// A GetSettings that returned defaults instead of reading the file would look
// fine until the user reopened the dialog.
func TestGetSettingsReadsBackWhatSaveSettingsWrote(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	tog := &stubToggler{}
	n := newNativeService(path, filepath.Join(home, "desktop.log"), tog)

	want := Settings{Port: "20001", LogLevel: "warn", LogFormat: "json", StartAtLogin: true}
	if err := n.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Level and format are the two the user must NOT have to restart for.
	if lvl := zerolog.GlobalLevel(); lvl != zerolog.WarnLevel {
		t.Errorf("global log level = %v after saving warn; the save did not take effect", lvl)
	}

	got, err := n.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.Port != want.Port || got.LogLevel != want.LogLevel || got.LogFormat != want.LogFormat {
		t.Errorf("read back %+v, want port/level/format from %+v", got, want)
	}
	if !got.StartAtLogin {
		t.Error("start at login not reported as enabled")
	}
	if got.ConfigPath != path {
		t.Errorf("ConfigPath = %q, want %q", got.ConfigPath, path)
	}
	if got.LogFilePath == "" {
		t.Error("LogFilePath is empty; the form has nothing to point Reveal at")
	}
	if got.EffectivePort != 0 {
		t.Errorf("EffectivePort = %d before boot reported one, want 0", got.EffectivePort)
	}
	// Once the boot goroutine reports the bound port, the form must see it —
	// this is the field that tells the user 19278 was taken.
	n.setEffectivePort(41000)
	got, err = n.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.EffectivePort != 41000 {
		t.Errorf("EffectivePort = %d, want 41000", got.EffectivePort)
	}
	if len(got.OverriddenByEnv) != 0 {
		t.Errorf("overrides reported with a clean environment: %v", got.OverriddenByEnv)
	}
	assertConfigRoundTrips(t, path, want.Port, want.LogLevel, want.LogFormat)
}

// The behaviour this whole task exists for: an exported KNOMIT_PORT beats the
// file, so GetSettings must report the env value AND name the variable. A
// GetSettings that dropped OverriddenByEnv would leave the form showing an
// editable port that Save cannot change.
func TestGetSettingsReportsEnvOverridesThatBeatTheFile(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})

	if err := applySettings(Settings{Port: "20002", LogLevel: "info", LogFormat: "console"}, path, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_PORT", "30000")
	t.Setenv("KNOMIT_LOG_LEVEL", "error")

	got, err := n.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.Port != "30000" {
		t.Errorf("Port = %q; the environment beats the file, so it must be 30000", got.Port)
	}
	if got.LogLevel != "error" {
		t.Errorf("LogLevel = %q, want error from the environment", got.LogLevel)
	}
	for _, want := range []string{"KNOMIT_PORT", "KNOMIT_LOG_LEVEL"} {
		if !slices.Contains(got.OverriddenByEnv, want) {
			t.Errorf("%s not reported as overriding the file; got %v", want, got.OverriddenByEnv)
		}
	}
	if slices.Contains(got.OverriddenByEnv, "KNOMIT_LOG_FORMAT") {
		t.Errorf("unset KNOMIT_LOG_FORMAT reported as an override: %v", got.OverriddenByEnv)
	}
}

// The full round trip the user actually performs, and the one applySettings'
// own tests cannot prove: SaveSettings must compute the override set from the
// LIVE environment and hand it down. A SaveSettings that passed nil would leave
// every applySettings test above green while still writing the environment's
// port into the user's file on the first save.
func TestSaveSettingsDoesNotPersistAnEnvOverriddenValue(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})

	// The file the user configured before exporting anything.
	if err := applySettings(Settings{Port: "20002", LogLevel: "info", LogFormat: "console"}, path, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_PORT", "30000")

	// Exactly what the form does: read, change ONLY the log level, save back.
	got, err := n.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.Port != "30000" {
		t.Fatalf("precondition: Port = %q, want the environment's 30000", got.Port)
	}
	got.LogLevel = "warn"
	if err := n.SaveSettings(got); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	if !strings.Contains(raw, `port = "20002"`) {
		t.Errorf("the user's configured port was overwritten by the environment:\n%s", raw)
	}
	if strings.Contains(raw, "30000") {
		t.Errorf("KNOMIT_PORT's value was persisted into knomit.toml:\n%s", raw)
	}
	if !strings.Contains(raw, `level = "warn"`) {
		t.Errorf("the edit the user actually made was not saved:\n%s", raw)
	}

	// And the file still loads to the port the user chose once the variable is
	// gone — the moment the bug would otherwise surface.
	os.Unsetenv("KNOMIT_PORT")
	assertConfigRoundTrips(t, path, "20002", "warn", "console")
}

// A knomit.toml carrying a literal level = "" is legal to config.Validate, so
// the dialog has to open on it — and whatever it shows must be something Save
// will accept. If GetSettings handed the form an empty level, validateSettings
// would refuse the very value it was given and the user would be stuck in a
// dialog they can only escape by hand-editing the file.
func TestGetSettingsHandsBackALevelSaveSettingsAccepts(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	if err := os.WriteFile(path, []byte("port = \"19278\"\n[log]\nlevel = \"\"\nformat = \"console\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})

	got, err := n.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.LogLevel == "" {
		t.Error("GetSettings returned an empty log level; the form has nothing to show and Save would reject it")
	}
	// The round trip the user performs: open, press Save, change nothing.
	if err := n.SaveSettings(got); err != nil {
		t.Fatalf("SaveSettings rejected what GetSettings returned: %v", err)
	}
}

// A save the config layer would reject at the next launch must be refused here,
// through the service, not just by the bare helper.
func TestSaveSettingsRefusesToWriteAnUnstartableConfig(t *testing.T) {
	home := desktopHome(t)
	path := filepath.Join(home, "knomit.toml")
	original := "port = \"19278\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	n := newNativeService(path, filepath.Join(home, "desktop.log"), &stubToggler{})

	if err := n.SaveSettings(Settings{Port: "19278", LogLevel: "loud", LogFormat: "console"}); err == nil {
		t.Fatal("SaveSettings accepted an invalid log level")
	}
	b, _ := os.ReadFile(path)
	if string(b) != original {
		t.Errorf("config was modified by a rejected save:\n%s", b)
	}
}

// assertConfigRoundTrips loads path through the real config layer and checks
// the three keys survived — proof the dialog wrote a file knomit can actually
// start from, which a substring check on the raw text cannot give.
func assertConfigRoundTrips(t *testing.T, path, port, level, format string) {
	t.Helper()
	t.Setenv("KNOMIT_HOME", filepath.Dir(path))
	for _, k := range envKeys {
		t.Setenv(k, "") // a developer's exported KNOMIT_PORT must not decide this
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the written config does not load: %v", err)
	}
	if cfg.Port != port {
		t.Errorf("loaded port = %q, want %q", cfg.Port, port)
	}
	if cfg.Log.Level != level {
		t.Errorf("loaded log level = %q, want %q", cfg.Log.Level, level)
	}
	if cfg.Log.Format != format {
		t.Errorf("loaded log format = %q, want %q", cfg.Log.Format, format)
	}
}
