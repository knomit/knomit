//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"knomit/internal/config"
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
func TestValidateSettings_RefusesABadExternalAddress(t *testing.T) {
	base := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}
	for _, bad := range []string{"http://kb.example:19279", "https://kb.example/git", "kb.example:19279", "https://", "https://u@kb.example"} {
		s := base
		s.ExternalAddresses = []string{"https://ok.example", bad}
		err := validateSettings(s)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: want a refusal naming it, got %v", bad, err)
		}
	}
	s := base
	s.ExternalAddresses = []string{"https://kb.example:19279", "HTTPS://kb.lan"}
	if err := validateSettings(s); err != nil {
		t.Errorf("good addresses refused: %v", err)
	}
}
