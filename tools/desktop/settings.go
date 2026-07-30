//go:build desktop

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/rs/zerolog"

	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/tomledit"
)

// Settings is what the Settings dialog reads and writes. The first four fields
// are editable; the rest is context the form needs in order to be honest about
// what it can and cannot change.
type Settings struct {
	Port         string `json:"port"`
	LogLevel     string `json:"logLevel"`
	LogFormat    string `json:"logFormat"`
	StartAtLogin bool   `json:"startAtLogin"`

	// EffectivePort is the port actually bound, which differs from Port
	// whenever the configured one was taken and the server fell back to an
	// ephemeral port. Zero until the boot goroutine reports it.
	EffectivePort int    `json:"effectivePort"`
	ConfigPath    string `json:"configPath"`
	LogFilePath   string `json:"logFilePath"`

	// OverriddenByEnv names the environment variables that currently beat
	// knomit.toml. Fields they control cannot be changed by writing the file,
	// so the form disables them rather than pretending otherwise.
	OverriddenByEnv []string `json:"overriddenByEnv"`
}

// envKeys are the environment variables that override the settings this dialog
// edits. Config layers defaults → knomit.toml → env (internal/config/config.go
// :263-306), so anything listed here wins over what this dialog writes: with
// KNOMIT_PORT exported, Save changes the file and nothing else — which is why
// the form has to be told.
//
// One entry per editable field. A field added to Settings without an entry here
// is a field the form will claim it can change when it cannot.
var envKeys = []string{"KNOMIT_PORT", "KNOMIT_LOG_LEVEL", "KNOMIT_LOG_FORMAT"}

// envOverrides returns the subset of envKeys that are set. getenv is injected
// so the behaviour is testable without mutating the process environment.
func envOverrides(getenv func(string) string) []string {
	var out []string
	for _, k := range envKeys {
		if getenv(k) != "" {
			out = append(out, k)
		}
	}
	return out
}

// validateSettings rejects anything that would fail config.Validate on the next
// launch (internal/config/config.go:382-391), plus the empty values config
// tolerates but a form field must not produce. The dialog must never be able to
// write a file that stops knomit from starting.
func validateSettings(s Settings) error {
	p, err := strconv.Atoi(s.Port)
	if err != nil {
		return fmt.Errorf("port must be a number, got %q", s.Port)
	}
	// Below 1024 needs root on Unix; knomit runs as the logged-in user.
	if p < 1024 || p > 65535 {
		return fmt.Errorf("port must be between 1024 and 65535, got %d", p)
	}
	// Empty is checked BEFORE ParseLevel because nothing downstream would catch
	// it: ParseLevel("") succeeds (it is zerolog's NoLevel, err == nil), and
	// config.Validate explicitly skips an empty level (config.go:389). A blanked
	// form field would therefore be persisted in silence — logging.Build then
	// quietly reads it as "info" (logging.go:21-23), so the user's chosen level
	// is gone with nothing to show for it. Refuse it here instead.
	if s.LogLevel == "" {
		return errors.New("log level must not be empty")
	}
	if _, err := zerolog.ParseLevel(s.LogLevel); err != nil {
		return fmt.Errorf("log level %q is not valid: %w", s.LogLevel, err)
	}
	if s.LogFormat != "console" && s.LogFormat != "json" {
		return fmt.Errorf("log format must be console or json, got %q", s.LogFormat)
	}
	return nil
}

// applySettings validates, then writes. The three config keys go to
// knomit.toml; start-at-login goes to the OS, because it is a login-item
// registration and not a knomit setting at all.
//
// Two backends means two ways to half-succeed, so the order is deliberate:
//
//  1. Validation happens BEFORE anything is touched, so a rejected save leaves
//     the file byte-identical and the login item alone.
//  2. The autostart toggle — the reversible half — goes next. If the OS refuses,
//     nothing has been written yet.
//  3. The file is written last, and atomically (temp file + rename), so a
//     failure mid-write cannot leave a truncated knomit.toml. If it fails
//     anyway, the autostart toggle is rolled back.
//
// The user therefore never ends up with the login item and the file disagreeing
// about what was saved.
func applySettings(s Settings, configPath string, tog autostart.Toggler) error {
	if err := validateSettings(s); err != nil {
		return err
	}

	// Read and edit in memory. A missing knomit.toml is normal — a fresh
	// install has none, and the macOS bundle ships none.
	src, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	for _, edit := range []struct{ table, key, value string }{
		{"", "port", s.Port},
		{"log", "level", s.LogLevel},
		{"log", "format", s.LogFormat},
	} {
		src, err = tomledit.SetString(src, edit.table, edit.key, edit.value)
		if err != nil {
			return fmt.Errorf("set %s: %w", edit.key, err)
		}
	}

	// The OS half. Enabled() failing is not fatal — the desired state is known
	// either way — but it does cost us the ability to roll back, since we no
	// longer know what to roll back to.
	prev, prevErr := tog.Enabled()
	rollback := false
	if prevErr != nil || prev != s.StartAtLogin {
		if err := setAutostart(tog, s.StartAtLogin); err != nil {
			return fmt.Errorf("start at login: %w", err)
		}
		rollback = prevErr == nil
	}

	if err := writeConfigFile(configPath, src); err != nil {
		if rollback {
			if rerr := setAutostart(tog, prev); rerr != nil {
				return fmt.Errorf("%w (and start at login could not be restored: %v)", err, rerr)
			}
		}
		return err
	}
	return nil
}

// setAutostart drives the toggler to the requested state.
func setAutostart(tog autostart.Toggler, on bool) error {
	if on {
		return tog.Enable()
	}
	return tog.Disable()
}

// configFileMode is what a dialog-created knomit.toml gets. Owner-only, because
// the file can hold an LLM API key ([llm] api_key). An existing file keeps
// whatever mode the user gave it.
const configFileMode fs.FileMode = 0o600

// writeConfigFile replaces path's contents atomically: a temp file in the same
// directory, then a rename. knomit.toml is read at every launch, so a
// half-written one is an app that will not start — os.WriteFile truncates
// first and would leave exactly that behind if the write failed part way.
func writeConfigFile(path string, data []byte) error {
	mode := configFileMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".knomit.toml-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmp := f.Name()
	// Harmless after a successful rename (the name is gone); the safety net is
	// for every path that returns early below.
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
