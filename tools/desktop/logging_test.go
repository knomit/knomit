//go:build desktop

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
)

// The desktop's defaults differ from the server's on purpose. The server logs
// to stdout with file rotation opt-in; a bundled desktop app has no usable
// stdout (LaunchServices points stderr at /dev/null), so the file is
// mandatory and console is the format a human will actually read.
func TestDesktopLogConfigFillsDefaults(t *testing.T) {
	got := desktopLogConfig(config.LogConfig{}, "/tmp/knomit-desktop.log")

	if got.Format != "console" {
		t.Errorf("Format = %q, want console", got.Format)
	}
	// Matches logging.Build's own default. The Settings dialog displays this
	// field, so leaving it empty would show the user a blank level that Save
	// then rejects.
	if got.Level != "info" {
		t.Errorf("Level = %q, want info", got.Level)
	}
	if got.File != "/tmp/knomit-desktop.log" {
		t.Errorf("File = %q, want the default log path", got.File)
	}
	if got.MaxSizeMB == 0 || got.MaxBackups == 0 || got.MaxAgeDays == 0 {
		t.Errorf("rotation defaults left at zero: %+v", got)
	}
}

// An operator who set log.format = "json" in knomit.toml means it. Defaults
// fill gaps; they never override.
func TestDesktopLogConfigKeepsExplicitValues(t *testing.T) {
	in := config.LogConfig{
		Format: "json", Level: "debug", File: "/custom/path.log",
		MaxSizeMB: 5, MaxBackups: 9, MaxAgeDays: 30,
	}
	got := desktopLogConfig(in, "/tmp/knomit-desktop.log")

	if got != in {
		t.Errorf("desktopLogConfig mutated an explicit config:\n got %+v\nwant %+v", got, in)
	}
}

// restoreGlobalLogger puts the process logger back on plain stderr once a test
// has pointed it at a temp file, so a later test never writes into a TempDir
// that has already been removed.
func restoreGlobalLogger(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		globalSink.swap(zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}), nil)
	})
}

// The log file the Logs window tails, the one "Reveal in Finder" opens, and the
// one the Settings dialog NAMES all come from here. They used to be resolved
// separately, and the version behind the window ignored `[log] file` entirely:
// configure one and the Logs window tails a path nothing writes to — blank
// forever, no error anywhere — while the dialog beside it shows the correct
// path. One resolver is the fix, and this is what says it honours the config.
func TestResolveLogFileHonoursTheConfiguredFile(t *testing.T) {
	cfg := config.Config{Log: config.LogConfig{File: "/custom/knomit.log"}}
	if got := resolveLogFile(cfg); got != "/custom/knomit.log" {
		t.Errorf("resolveLogFile = %q, want the CONFIGURED file; the Logs window would tail the wrong path", got)
	}
}

// With no [log] file the desktop default is what everything agrees on — and it
// must be the same string bootstrapLogging wrote to, or phase two moves the log
// mid-launch.
func TestResolveLogFileFallsBackToTheDesktopDefault(t *testing.T) {
	got := resolveLogFile(config.Config{})
	if got != defaultLogPath() {
		t.Errorf("resolveLogFile = %q, want the desktop default %q", got, defaultLogPath())
	}
	// bootstrapLogging writes to this same basename before config is read. If
	// the two ever diverge, phase two silently moves the log mid-launch and the
	// first seconds of every startup end up in a file nothing tails.
	if got != "" && filepath.Base(got) != "knomit-desktop.log" {
		t.Errorf("default log file = %q, want the basename bootstrapLogging uses", got)
	}
}

// applyLogConfig runs from SaveSettings, on a Wails IPC goroutine, while the
// server and 60-odd packages under internal/ are logging through zerolog's
// global logger. That global is a bare package-level var, not an atomic: the
// `log.Logger = lg` this used to do was an unsynchronised write racing every
// one of those readers.
//
// Under `-race` this test fails on the old code and passes on the new. It is
// the only thing that drives both sides at once, which is why the race survived
// eleven task reviews.
func TestApplyLogConfigIsSafeWhileLogging(t *testing.T) {
	restoreGlobalLogger(t)
	path := filepath.Join(t.TempDir(), "race.log")

	// applyLogConfig always builds its console sink over os.Stderr, and this
	// test writes thousands of records; without this the real signal is buried
	// in CI output. Swapped BEFORE any goroutine starts and restored AFTER they
	// have all joined, so the swap itself is not a race.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	realStderr := os.Stderr
	os.Stderr = devnull

	// Two groups, not one: the writers run until the appliers are finished, so
	// waiting on a single group would wait on goroutines that are waiting to be
	// told to stop.
	var writers, appliers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				log.Info().Str("k", "v").Msg("concurrent write")
			}
		}()
	}

	// The formats alternate so the swap really does replace the writer stack,
	// not just rebuild an identical one.
	for i := 0; i < 4; i++ {
		appliers.Add(1)
		go func(i int) {
			defer appliers.Done()
			format := "console"
			if i%2 == 0 {
				format = "json"
			}
			for n := 0; n < 25; n++ {
				if err := applyLogConfig(config.LogConfig{Level: "info", Format: format}, path); err != nil {
					t.Errorf("applyLogConfig: %v", err)
					return
				}
			}
		}(i)
	}

	appliers.Wait()
	close(stop)
	writers.Wait()

	os.Stderr = realStderr
	_ = devnull.Close()
}

// The one write to log.Logger happens at startup and never again — that is the
// whole basis for the swap above being safe. If a later change reintroduced an
// assignment, the logger would stop being the one built over globalSink and
// swapping would silently stop having any effect.
func TestApplyLogConfigKeepsTheLoggerBoundToTheSink(t *testing.T) {
	restoreGlobalLogger(t)
	path := filepath.Join(t.TempDir(), "bound.log")

	if err := applyLogConfig(config.LogConfig{Level: "info", Format: "console"}, path); err != nil {
		t.Fatalf("applyLogConfig: %v", err)
	}
	log.Info().Msg("first sink")

	// A second apply must redirect the SAME logger, without reassigning it.
	second := filepath.Join(t.TempDir(), "second.log")
	if err := applyLogConfig(config.LogConfig{Level: "info", Format: "console"}, second); err != nil {
		t.Fatalf("applyLogConfig: %v", err)
	}
	log.Info().Msg("second sink")

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first log: %v", err)
	}
	if !strings.Contains(string(first), "first sink") {
		t.Errorf("first sink missing its line:\n%s", first)
	}
	if strings.Contains(string(first), "second sink") {
		t.Errorf("the swap did not take effect; the old file still receives writes:\n%s", first)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second log: %v", err)
	}
	if !strings.Contains(string(b), "second sink") {
		t.Errorf("second sink missing its line:\n%s", b)
	}
}

// Each apply opens a fresh lumberjack.Logger over the same file. Without the
// swap closing the outgoing one, every Settings save leaks an open descriptor
// for the life of the process. Asserting the fd count is not portable, so this
// asserts the mechanism: the sink hands its closer over and closes what it held.
func TestLogSinkClosesTheOutgoingRotator(t *testing.T) {
	sink := newLogSink(zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}))
	first := &countingCloser{}
	second := &countingCloser{}

	sink.swap(zerolog.MultiLevelWriter(os.Stderr), first)
	if first.closed != 0 {
		t.Fatalf("the INCOMING rotator was closed: %d", first.closed)
	}
	sink.swap(zerolog.MultiLevelWriter(os.Stderr), second)
	if first.closed != 1 {
		t.Errorf("outgoing rotator closed %d times, want exactly 1 (an fd leak per Settings save)", first.closed)
	}
	if second.closed != 0 {
		t.Errorf("the incoming rotator was closed: %d", second.closed)
	}
}

type countingCloser struct{ closed int }

func (c *countingCloser) Close() error { c.closed++; return nil }
