//go:build desktop

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rs/zerolog"

	"knomit/internal/config"
	"knomit/tools/desktop/internal/autostart"
	"knomit/tools/desktop/internal/tomledit"
)

// Settings is what the Settings dialog reads and writes. The first five fields
// are editable; the rest is context the form needs in order to be honest about
// what it can and cannot change.
type Settings struct {
	Port         string `json:"port"`
	LogLevel     string `json:"logLevel"`
	LogFormat    string `json:"logFormat"`
	StartAtLogin bool   `json:"startAtLogin"`
	// TLSAddr is [tls].addr: where the mTLS listener for enrolled fleet peers
	// binds, "" = off. Like Port it is bound once at boot, so a change needs
	// the restart the form offers (knomit#256).
	TLSAddr string `json:"tlsAddr"`
	// ExternalAddresses is the root external_addresses: the https:// URLs
	// fleet peers reach this instance's Fleet port at (config F10). A fleet
	// hosted here is refused without one (addresses_required). A reload does
	// not apply it (it is listed restart_needed, decision serve/reload), so
	// the form restarts the server after a save that changed it. No
	// environment variable overrides it.
	ExternalAddresses []string `json:"externalAddresses"`
	// HTTPScope is who may reach the plaintext HTTP port: "off" (http =
	// false, socket only), "local" (this computer only: a loopback host) or
	// "all" (every network interface). "other" is what GetSettings reports
	// for a knomit.toml that binds some other host (a LAN address, a DNS
	// name): Settings does not edit that, and saving "other" changes nothing.
	// Empty on a save means "leave http and host alone".
	HTTPScope string `json:"httpScope"`

	// Server is the running server's own account of itself, from server.json
	// and the supervisor. Read-only context: a save ignores it.
	Server ServerState `json:"server"`

	// EffectivePort is the plaintext port the running server says it bound
	// (its server.json `port`). Zero while no server has written the file, or
	// when the server has no TCP listener.
	EffectivePort int    `json:"effectivePort"`
	ConfigPath    string `json:"configPath"`
	LogFilePath   string `json:"logFilePath"`

	// OverriddenByEnv names the environment variables that currently beat
	// knomit.toml. Fields they control cannot be changed by writing the file,
	// so the form disables them rather than pretending otherwise.
	OverriddenByEnv []string `json:"overriddenByEnv"`
}

// Scopes of the plaintext HTTP port (Settings.HTTPScope).
const (
	scopeOff   = "off"
	scopeLocal = "local"
	scopeAll   = "all"
	scopeOther = "other"
)

// hostIsAllInterfaces: the hosts that bind every interface. An empty host is
// ":port" to net.Listen, which is all of them.
func hostIsAllInterfaces(h string) bool {
	switch strings.Trim(strings.ToLower(h), "[]") {
	case "", "0.0.0.0", "::":
		return true
	}
	return false
}

// hostIsLoopback: the hosts that bind this computer only.
func hostIsLoopback(h string) bool {
	switch strings.Trim(strings.ToLower(h), "[]") {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// scopeOf is the scope http and host add up to.
func scopeOf(http bool, host string) string {
	switch {
	case !http:
		return scopeOff
	case hostIsAllInterfaces(host):
		return scopeAll
	case hostIsLoopback(host):
		return scopeLocal
	}
	return scopeOther
}

// configKeys is the single source of truth for the knomit.toml keys this
// dialog edits: where each one lives in the file, how to read it out of
// Settings, and which environment variable overrides it.
//
// Config layers defaults → knomit.toml → env (internal/config/config.go
// :263-306), so an exported variable wins over anything this dialog writes.
//
// ONE table, deliberately. envOverrides reports overrides from it and
// applySettings declines to WRITE from it, so "which variable owns which key"
// cannot drift between what the form is told it cannot change and what the
// writer actually refuses to change. Two lists would let those two disagree,
// and the disagreement is invisible until a user's knomit.toml has quietly
// grown a value they never typed.
//
// One entry per editable field. A field added to Settings without an entry here
// is a field the form will claim it can change when it cannot.
//
// optional marks a key whose empty value means the same as its absence
// ([tls].addr: "" and missing are both "off"). Such a key is not CREATED
// empty: otherwise every save, whatever it changed, would append it to a
// file that never had it.
var configKeys = []struct {
	env      string
	table    string
	key      string
	value    func(Settings) string
	optional bool
	// scope marks the keys the HTTP scope owns (http, host). They are written
	// by scopeEdits, not by the string loop, because one choice sets up to two
	// keys and a key is written only when the scope moves; the entries are
	// here so the environment variables that beat them are reported (and the
	// scope refused) from the same table.
	scope bool
}{
	{env: "KNOMIT_PORT", table: "", key: "port", value: func(s Settings) string { return s.Port }},
	{env: "KNOMIT_LOG_LEVEL", table: "log", key: "level", value: func(s Settings) string { return s.LogLevel }},
	{env: "KNOMIT_LOG_FORMAT", table: "log", key: "format", value: func(s Settings) string { return s.LogFormat }},
	{env: "KNOMIT_TLS_ADDR", table: "tls", key: "addr", value: func(s Settings) string { return s.TLSAddr }, optional: true},
	{env: "KNOMIT_HTTP", table: "", key: "http", scope: true},
	{env: "KNOMIT_HOST", table: "", key: "host", scope: true},
}

// envKeys are the environment variables in configKeys, derived so the two can
// never disagree.
var envKeys = func() []string {
	out := make([]string, len(configKeys))
	for i, c := range configKeys {
		out[i] = c.env
	}
	return out
}()

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
	switch s.HTTPScope {
	case "", scopeOff, scopeLocal, scopeAll, scopeOther:
	default:
		return fmt.Errorf("http scope must be off, local or all, got %q", s.HTTPScope)
	}
	// external_addresses is checked by applySettings, and only when the list
	// changed: a hand-written entry config accepts (say http://) must not
	// block a save of something else.
	return validateTLSAddr(s.TLSAddr, p)
}

// validateExternalAddresses applies config's own check to each entry (what
// the next boot's config.Validate would refuse), plus https://: members
// reach the hub's mTLS listener over https, and an enroll refuses any other
// scheme (address_not_https), so the form must not save one.
func validateExternalAddresses(addrs []string) error {
	for _, a := range addrs {
		if err := config.ValidateExternalAddress(a); err != nil {
			return fmt.Errorf("external address %q %v (want https://host[:port], the address peers use)", a, err)
		}
		if !strings.HasPrefix(strings.ToLower(a), "https://") {
			return fmt.Errorf("external address %q is not https:// (members reach the Fleet port over https)", a)
		}
	}
	return nil
}

// validateTLSAddr checks [tls].addr as host:port — the host optional (all
// interfaces), IPv6 bracketed — with the port rules Port has, and not the
// plaintext port. config does not validate it at all: this is the only check
// before a listener that would fail at the next boot. "" is "off".
func validateTLSAddr(addr string, plainPort int) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || strings.ContainsAny(host, " /") {
		return fmt.Errorf("the fleet listener address must be host:port (e.g. 0.0.0.0:19279), got %q", addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("the fleet listener port must be a number, got %q", port)
	}
	if p < 1024 || p > 65535 {
		return fmt.Errorf("the fleet listener port must be between 1024 and 65535, got %d", p)
	}
	if p == plainPort {
		return fmt.Errorf("the fleet listener cannot share port %d with the local server", p)
	}
	return nil
}

// applySettings validates, then writes. The three config keys go to
// knomit.toml; start-at-login goes to the OS, because it is a login-item
// registration and not a knomit setting at all.
//
// overriddenByEnv names the variables currently beating the file (see
// envOverrides); the keys they own are validated but NOT written. Every field
// is still validated, because the form sends all of them and a bad value is a
// bad value whatever is going to be persisted.
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
func applySettings(s Settings, configPath string, tog autostart.Toggler, overriddenByEnv []string) error {
	if err := validateSettings(s); err != nil {
		return err
	}

	// Read and edit in memory. A missing knomit.toml is normal — a fresh
	// install has none, and the macOS bundle ships none.
	src, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	for _, c := range configKeys {
		if c.scope {
			continue
		}
		// An env-owned key is never written, neither updated nor created.
		// GetSettings reports values from AFTER the env layer, so s carries the
		// ENVIRONMENT's value for an overridden field — not the file's. Writing
		// it would persist a value the user never typed, cannot see in the
		// disabled field, and will not discover until they unset the variable
		// and find knomit still on the old setting. The form tells them "saving
		// cannot change this"; this loop is what makes that true.
		if slices.Contains(overriddenByEnv, c.env) {
			continue
		}
		if c.optional && c.value(s) == "" && !tomledit.Has(src, c.table, c.key) {
			continue
		}
		src, err = tomledit.SetString(src, c.table, c.key, c.value(s))
		if err != nil {
			return fmt.Errorf("set %s: %w", c.key, err)
		}
	}
	src, err = scopeEdits(src, s.HTTPScope, overriddenByEnv)
	if err != nil {
		return err
	}
	// external_addresses is a string array, so it is written here rather
	// than by the string loop over configKeys; no environment variable owns
	// it. It is written ONLY when the list differs from the file's: a save
	// of anything else leaves it byte for byte, so an array the user spelled
	// over several lines (which tomledit's one-line rewrite would break, and
	// the decode check below would then refuse) never blocks an unrelated
	// save, and an empty list is not created in a file without the key.
	// The same rule decides whether the list is checked: a changed list must
	// pass validateExternalAddresses (nothing has been written yet).
	if !slices.Equal(s.ExternalAddresses, fileExternalAddresses(src)) {
		if err := validateExternalAddresses(s.ExternalAddresses); err != nil {
			return err
		}
		src, err = tomledit.SetStringArray(src, "", "external_addresses", s.ExternalAddresses)
		if err != nil {
			return fmt.Errorf("set external_addresses: %w", err)
		}
	}
	// tomledit edits text within a subset of TOML (see its package doc): on a
	// file outside it — an inline `tls = { … }`, say — an edit can produce a
	// file that no longer parses, i.e. a knomit that no longer starts. Decode
	// the result with config's own parser into config's own struct first, and
	// refuse rather than write it. Nothing has been touched yet.
	if _, err := toml.Decode(string(src), &config.Config{}); err != nil {
		return fmt.Errorf("the edited %s would not load, so it was not saved (edit the file by hand): %w", filepath.Base(configPath), err)
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

// fileExternalAddresses is external_addresses as src spells it (nil when
// absent, or when src does not decode: then the write goes ahead and the
// decode check after it decides).
func fileExternalAddresses(src []byte) []string {
	var f struct {
		ExternalAddresses []string `toml:"external_addresses"`
	}
	if _, err := toml.Decode(string(src), &f); err != nil {
		return nil
	}
	return f.ExternalAddresses
}

// scopeEdits moves the plaintext HTTP port to the requested scope, writing
// ONLY the keys that have to change to get there: a scope the file already
// means writes nothing (no `http = true` / `host = "127.0.0.1"` noise in a
// file that never had them), "off" touches http alone, and "local" over a
// loopback host leaves host as the user spelled it. Neither key is written
// while its environment variable owns it (the scope is refused whole: half a
// scope would be a surprise), and "other" / "" are never written.
func scopeEdits(src []byte, want string, overriddenByEnv []string) ([]byte, error) {
	if want == "" || want == scopeOther {
		return src, nil
	}
	for _, c := range configKeys {
		if c.scope && slices.Contains(overriddenByEnv, c.env) {
			return src, nil
		}
	}
	// The file as the server will read it, over the defaults: absent keys mean
	// their defaults (http true, host localhost).
	cur := config.Defaults()
	if _, err := toml.Decode(string(src), &cur); err != nil {
		return nil, fmt.Errorf("the existing knomit.toml would not load, so it was not edited (fix it by hand): %w", err)
	}
	var err error
	setHTTP := func(on bool) {
		if err == nil && cur.HTTP != on {
			src, err = tomledit.SetBool(src, "", "http", on)
		}
	}
	setHost := func(host string) {
		if err == nil {
			src, err = tomledit.SetString(src, "", "host", host)
		}
	}
	switch want {
	case scopeOff:
		setHTTP(false)
	case scopeLocal:
		setHTTP(true)
		if !hostIsLoopback(cur.Host) {
			setHost("127.0.0.1")
		}
	case scopeAll:
		setHTTP(true)
		if !hostIsAllInterfaces(cur.Host) {
			setHost("0.0.0.0")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("set http scope: %w", err)
	}
	return src, nil
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
//
// On Windows the mode is meaningless: Go maps it onto the read-only attribute
// alone, and the file is private because it inherits the data root's DACL
// (internal/platform/privdir, applied at boot) through the temp file the
// rename carries in. The "keep the user's mode" Stat in writeConfigFile reads
// 0666 there for any writable file and carries nothing.
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
