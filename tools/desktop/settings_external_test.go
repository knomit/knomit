//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"knomit/internal/foundation/config"
)

// The External address field (2026-10-17, PR #545 option a): a save writes
// the root external_addresses that config loads, before the first table, and
// the next boot reads exactly the list the form held.
// SABOTAGE: skip the external_addresses write -> the loaded list is empty -> red.
func TestApplySettings_WritesExternalAddresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	if err := os.WriteFile(path, []byte("port = \"19278\"\n\n[tls]\naddr = \"0.0.0.0:19279\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Settings{Port: "19278", LogLevel: "info", LogFormat: "console", TLSAddr: "0.0.0.0:19279",
		ExternalAddresses: []string{"https://kb-01.example:19279", "https://kb-01.lan:19279"}}
	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	b, _ := os.ReadFile(path)
	var cfg config.Config
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		t.Fatalf("written file does not parse: %v\n%s", err, b)
	}
	if got := strings.Join(cfg.ExternalAddresses, ","); got != "https://kb-01.example:19279,https://kb-01.lan:19279" {
		t.Fatalf("external_addresses = %q, want both, in order\n%s", got, b)
	}
	if cfg.TLS.Addr != "0.0.0.0:19279" {
		t.Fatalf("[tls].addr lost: %q\n%s", cfg.TLS.Addr, b)
	}

	// Cleared: written as [] (the key existed), not left at the old list.
	s.ExternalAddresses = nil
	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatalf("applySettings (cleared): %v", err)
	}
	b, _ = os.ReadFile(path)
	cfg = config.Config{}
	if _, err := toml.Decode(string(b), &cfg); err != nil || len(cfg.ExternalAddresses) != 0 {
		t.Fatalf("cleared list: err %v, %v\n%s", err, cfg.ExternalAddresses, b)
	}
}

// A hand-written external_addresses spelled over several lines (valid TOML)
// must not block a save of anything else: the list is written only when it
// changed, so an unrelated save leaves those lines byte for byte. PR #545
// review F2.
// SABOTAGE: write the list whenever it is non-empty -> the one-line rewrite
// leaves the continuation lines, the decode check refuses the save -> red.
func TestApplySettings_MultiLineExternalAddressesDoNotBlockAnUnrelatedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	block := "external_addresses = [\n  \"https://a.example:19279\",\n  \"https://b.example:19279\",\n]\n"
	src := "port = \"19278\"\n" + block + "\n[tls]\naddr = \"0.0.0.0:19279\"\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// What GetSettings hands the form for this file; only the level changes.
	s := Settings{Port: "19278", LogLevel: "debug", LogFormat: "console", TLSAddr: "0.0.0.0:19279",
		ExternalAddresses: []string{"https://a.example:19279", "https://b.example:19279"}}
	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatalf("an unrelated save was refused: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), block) || !strings.Contains(string(b), `level = "debug"`) {
		t.Fatalf("the multi-line list must be left as written and the level saved:\n%s", b)
	}
}

// A file that never had the key does not grow `external_addresses = []` on
// an unrelated save.
// SABOTAGE: write the key unconditionally -> red.
func TestApplySettings_EmptyExternalAddressesCreateNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	s := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}
	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "external_addresses") {
		t.Fatalf("an empty list created the key:\n%s", b)
	}
}

// What the next boot's config.Validate refuses, and any scheme but https://
// (an enroll refuses it: address_not_https), is refused before the file is
// touched.
// SABOTAGE: drop the https:// check -> the http row passes -> red; drop the
// config check -> the path row passes -> red.
func TestApplySettings_RefusesABadExternalAddress(t *testing.T) {
	base := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}
	for _, bad := range []string{"http://kb.example:19279", "https://kb.example/git", "kb.example:19279", "https://", "https://u@kb.example"} {
		path := filepath.Join(t.TempDir(), "knomit.toml")
		s := base
		s.ExternalAddresses = []string{"https://ok.example", bad}
		err := applySettings(s, path, &stubToggler{}, nil)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: want a refusal naming it, got %v", bad, err)
		}
		if _, serr := os.Stat(path); serr == nil {
			t.Errorf("%q: a refused save wrote the file", bad)
		}
	}
	s := base
	s.ExternalAddresses = []string{"https://kb.example:19279", "HTTPS://kb.lan"}
	if err := applySettings(s, filepath.Join(t.TempDir(), "knomit.toml"), &stubToggler{}, nil); err != nil {
		t.Errorf("good addresses refused: %v", err)
	}
}

// A hand-written entry config accepts but the field would not (http://) is
// checked only when the list changes: a save of something else goes through
// and leaves it as written (PR #545 review, probe 2; same rule as F2).
// SABOTAGE: validate the list whatever changed -> the level save is refused -> red.
func TestApplySettings_UnchangedHandWrittenAddressDoesNotBlockAnUnrelatedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knomit.toml")
	if err := os.WriteFile(path, []byte("port = \"19278\"\nexternal_addresses = [\"http://kb.example:19279\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Settings{Port: "19278", LogLevel: "debug", LogFormat: "console", ExternalAddresses: []string{"http://kb.example:19279"}}
	if err := applySettings(s, path, &stubToggler{}, nil); err != nil {
		t.Fatalf("an unrelated save was refused: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `external_addresses = ["http://kb.example:19279"]`) || !strings.Contains(string(b), `level = "debug"`) {
		t.Fatalf("file:\n%s", b)
	}
}
