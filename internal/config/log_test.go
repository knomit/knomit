package config

import (
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
