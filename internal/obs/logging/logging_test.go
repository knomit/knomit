package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A desktop bundle's stderr goes to /dev/null under LaunchServices, so the FILE
// is the only log output a user ever sees. With Format=="console" it must hold
// human-readable records, not the raw JSON the file sink used to get
// unconditionally.
func TestBuildConsoleFormatWritesHumanReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lc := config.LogConfig{
		Format: "console", Level: "info", File: path,
		MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1,
	}

	lg, _, err := Build(lc, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Str("port", "19278").Msg("server up")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	got := string(b)

	if strings.Contains(got, `"level":"info"`) {
		t.Errorf("file holds raw JSON, want console format:\n%s", got)
	}
	if !strings.Contains(got, "INF") || !strings.Contains(got, "server up") {
		t.Errorf("file missing a console-formatted record:\n%s", got)
	}
	if !strings.Contains(got, "port=19278") {
		t.Errorf("file dropped the structured field:\n%s", got)
	}
	// NoColor is load-bearing: ConsoleWriter colorises by default, and a file
	// full of ANSI escapes is worse to read than the JSON it replaced.
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("file contains ANSI escapes; NoColor was not set:\n%q", got)
	}
}

// The file keeps days of history (MaxAge is in days, and backups are kept), so
// every line has to say WHICH day it is from. ConsoleWriter's default is
// time.Kitchen — "5:01PM" — which cannot distinguish today's lines from
// Monday's, and the file is the only log surface a macOS bundle has.
//
// The second assertion is not cosmetic: the Logs window reads a console line as
// `<stamp> <LVL> <message>` (ui/src/LogView.tsx parseLine), so a dated-but-
// spaced format like "Jan 2 15:04:05" would fix the date and silently break the
// window's level filter — an unparseable line is treated as unrankable and
// shown at every threshold, so the filter stops filtering rather than emptying.
func TestFileSinkTimestampIsDatedAndSpaceFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lc := config.LogConfig{
		Format: "console", Level: "info", File: path,
		MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1,
	}

	lg, _, err := Build(lc, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("server up")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	line := strings.TrimRight(string(b), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("log line has too few tokens to check: %q", line)
	}

	stamp := fields[0]
	if _, perr := time.Parse(fileTimeFormat, stamp); perr != nil {
		t.Errorf("first token %q is not a %s timestamp (%v); the file must carry a DATE, not time.Kitchen",
			stamp, fileTimeFormat, perr)
	}
	if !strings.Contains(stamp, time.Now().Format("2006-01-02")) {
		t.Errorf("timestamp %q carries no date; a week of backups would be indistinguishable", stamp)
	}
	if fields[1] != "INF" {
		t.Errorf("level token = %q, want INF as the SECOND token: the Logs window's parseLine reads fields[1], "+
			"so a timestamp containing whitespace breaks it. Line: %q", fields[1], line)
	}
}

// The stderr sink is a developer watching a terminal: short is right there, and
// the date is never in doubt. Pinning it keeps the file's dated format from
// being "fixed" onto both writers.
func TestConsoleSinkKeepsTheShortClockTime(t *testing.T) {
	var console bytes.Buffer
	lc := config.LogConfig{Format: "console", Level: "info"}

	lg, _, err := Build(lc, &console, io.Discard, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("hi")

	// The stderr writer colorises (unlike the file writer's NoColor), so the
	// timestamp arrives wrapped in ANSI dim/reset escapes.
	stamp := stripANSI(strings.Fields(console.String())[0])
	if _, perr := time.Parse(time.Kitchen, stamp); perr != nil {
		t.Errorf("stderr timestamp %q is not time.Kitchen (%v)", stamp, perr)
	}
}

// stripANSI removes SGR escape sequences ("\x1b[...m") so a colorised token can
// be compared as text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// A file sink owns an OS file descriptor, and the desktop rebuilds its logger
// every time Settings is saved. Without a handle to close, each save leaks one.
func TestBuildWriterReturnsTheFileRotatorToClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lc := config.LogConfig{Format: "console", Level: "info", File: path, MaxSizeMB: 1}

	_, closer, _, err := BuildWriter(lc, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("BuildWriter: %v", err)
	}
	if closer == nil {
		t.Fatal("BuildWriter returned no closer for a configured file sink; every reconfiguration would leak an fd")
	}
	if cerr := closer.Close(); cerr != nil {
		t.Errorf("Close: %v", cerr)
	}

	// No file, nothing to close — a nil closer, not a closer over nothing.
	if _, c, _, berr := BuildWriter(config.LogConfig{Level: "info"}, io.Discard, io.Discard, nil); berr != nil || c != nil {
		t.Errorf("BuildWriter(no file) = closer %v, err %v; want nil, nil", c, berr)
	}
}

// The json format is what containers and the server use. Wrapping the file sink
// must not touch it.
func TestBuildJSONFormatKeepsTheFileStructured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.log")
	lc := config.LogConfig{
		Format: "json", Level: "info", File: path,
		MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1,
	}

	lg, _, err := Build(lc, io.Discard, io.Discard, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	lg.Info().Msg("server up")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(b), `"message":"server up"`) {
		t.Errorf("json format lost its structured file sink:\n%s", b)
	}
}
