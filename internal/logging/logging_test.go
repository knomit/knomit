package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"knomit/internal/config"
)

func TestBuildLogger_JSONFormatWritesStructured(t *testing.T) {
	var console, jsonOut bytes.Buffer
	lc := config.LogConfig{Format: "json", Level: "info"}

	lg, lvl, err := Build(lc, &console, &jsonOut, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if lvl != zerolog.InfoLevel {
		t.Errorf("level = %v, want info", lvl)
	}
	lg.Info().Str("k", "v").Msg("hello")

	if console.Len() != 0 {
		t.Errorf("json format wrote to console sink: %q", console.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, jsonOut.String())
	}
	if rec["k"] != "v" || rec["message"] != "hello" {
		t.Errorf("unexpected JSON record: %v", rec)
	}
}

func TestBuildLogger_ConsoleFormatUsesConsoleSink(t *testing.T) {
	var console, jsonOut bytes.Buffer
	lc := config.LogConfig{Format: "console", Level: "info"}

	lg, _, err := Build(lc, &console, &jsonOut, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("hi")

	if jsonOut.Len() != 0 {
		t.Errorf("console format wrote to json sink: %q", jsonOut.String())
	}
	if !strings.Contains(console.String(), "hi") {
		t.Errorf("console sink missing message: %q", console.String())
	}
}

func TestBuildLogger_FileSinkReceivesOutput(t *testing.T) {
	var console, jsonOut bytes.Buffer
	file := filepath.Join(t.TempDir(), "knomit.log")
	lc := config.LogConfig{Format: "console", Level: "info", File: file, MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1}

	lg, _, err := Build(lc, &console, &jsonOut, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("to-file")

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(raw), "to-file") {
		t.Errorf("file sink missing message: %q", raw)
	}
}

func TestBuildLogger_RingIsTeed(t *testing.T) {
	var console, jsonOut bytes.Buffer
	var ring bytes.Buffer
	lc := config.LogConfig{Format: "console", Level: "info"}

	lg, _, err := Build(lc, &console, &jsonOut, &ring)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("teed")

	if !strings.Contains(ring.String(), "teed") {
		t.Errorf("ring did not receive the log line: %q", ring.String())
	}
}

func TestBuildLogger_RejectsBadLevel(t *testing.T) {
	var console, jsonOut bytes.Buffer
	lc := config.LogConfig{Format: "console", Level: "loud"}
	if _, _, err := Build(lc, &console, &jsonOut, nil); err == nil {
		t.Fatal("Build must reject an unparseable level")
	}
}
