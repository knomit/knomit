package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults_LogConfig(t *testing.T) {
	l := Defaults().Log
	if l.Format != "console" {
		t.Errorf("Log.Format: want console, got %q", l.Format)
	}
	if l.Level != "info" {
		t.Errorf("Log.Level: want info, got %q", l.Level)
	}
	if l.File != "" {
		t.Errorf("Log.File: want empty (stdout default), got %q", l.File)
	}
	if l.MaxSizeMB != 10 || l.MaxBackups != 3 || l.MaxAgeDays != 7 {
		t.Errorf("Log rotation defaults: got size=%d backups=%d age=%d, want 10/3/7",
			l.MaxSizeMB, l.MaxBackups, l.MaxAgeDays)
	}
	if l.SlowRequestMS != 1000 {
		t.Errorf("Log.SlowRequestMS: want 1000, got %d", l.SlowRequestMS)
	}
	// 50 for a structural reason: a trigger's `if` is capped at 100 ms by the
	// sandbox, so a condition that has spent half its budget is worth naming;
	// the 1000 ms request default could never fire under that cap.
	if l.SlowTriggerMS != 50 {
		t.Errorf("Log.SlowTriggerMS: want 50, got %d", l.SlowTriggerMS)
	}
}

// SlowTriggerThresholdFromConfig, the config half: the TOML key is read, the
// env var overrides it, and 0 is a legitimate value (it disables the detector;
// the dispatcher test covers the disabling).
func TestLoad_SlowTriggerMS_TOMLThenEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "knomit.toml"), []byte("[log]\nslow_trigger_ms = 40\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KNOMIT_HOME", dir)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.SlowTriggerMS != 40 {
		t.Fatalf("TOML slow_trigger_ms: want 40, got %d", cfg.Log.SlowTriggerMS)
	}
	t.Setenv("KNOMIT_LOG_SLOW_TRIGGER_MS", "0")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Log.SlowTriggerMS != 0 {
		t.Fatalf("KNOMIT_LOG_SLOW_TRIGGER_MS must override the TOML value, got %d", cfg.Log.SlowTriggerMS)
	}
}

func TestValidate_RejectsBadLogFormat(t *testing.T) {
	cfg := Defaults()
	cfg.Log.Format = "xml"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() must reject an unknown log format")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error %q should mention format", err.Error())
	}
}

func TestValidate_RejectsBadLogLevel(t *testing.T) {
	cfg := Defaults()
	cfg.Log.Level = "loud"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() must reject an unparseable log level")
	}
}

func TestValidate_AcceptsJSONFormatAndKnownLevels(t *testing.T) {
	cfg := Defaults()
	cfg.Log.Format = "json"
	cfg.Log.Level = "debug"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a valid log config: %v", err)
	}
}
