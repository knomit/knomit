package logging

import (
	"io"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Options is everything the log sink needs, owned by logging itself.
//
// Build and BuildWriter used to take config.LogConfig directly, which was the
// ONE edge from internal/platform out to a knomit package — and so the one
// exception TestPlatformKnowsNothingAboutKnomit would have had to carry.
// Exception lists are how a layering rule stops meaning anything, so the
// dependency is inverted instead: logging declares what it needs, and knows
// nothing about where the values came from.
//
// Exactly one function translates: app.LoggingOptions, in internal/app, which
// both binaries that build a logger already import. Its doc explains why the
// converter can live in neither of the packages it names. Keep it the only
// one — a second converter reintroduces the failure this arrangement removes,
// where a rotation key wired at one call site and not the other takes effect
// in one binary and vanishes in the other.
//
// The fields are deliberately a SUBSET of config.LogConfig. SlowRequestMS and
// CrashFile live in that struct too and are read elsewhere — by the HTTP
// middleware and by crashdump — never here, and listing them would invite a
// future editor to wire them up in the wrong place.
type Options struct {
	// Format is "console" (human, stderr — default) or "json" (structured).
	Format string
	// Level is a zerolog level name (trace/debug/info/warn/error/...).
	// Empty means "info"; an unparseable value is an error.
	Level string
	// File, when non-empty, adds a rotating file sink (lumberjack).
	File string
	// MaxSizeMB, MaxBackups and MaxAgeDays are lumberjack's rotation keys.
	// They are read only when File is set.
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
}

// fileTimeFormat is what the human-readable FILE sink stamps on each line.
//
// It exists because ConsoleWriter's default is time.Kitchen ("5:01PM"), which
// is right for a developer watching a terminal and useless in a file: with
// MaxAge measured in days and backups kept, a log full of bare clock times
// gives no way to tell today's lines from Monday's. The file is the ONLY log
// surface a macOS bundle has (LaunchServices points its stderr at /dev/null),
// so this is the timestamp most users will ever read.
//
// RFC3339 specifically, and not a "Jan 2 15:04:05" style: the Logs window reads
// a console-formatted line as `<stamp> <LVL> <message>` (ui/src/LogView.tsx
// parseLine), so a timestamp containing a space would shift the level out of
// the second position and silently break the window's level filter. RFC3339 has
// no spaces. TestFileSinkTimestampIsDatedAndSpaceFree pins that.
//
// This constraint is the CONSOLE sink's alone. parseLine also understands the
// json format, which carries its level as a field and so cannot be broken this
// way — but json is not what this writer emits, and a space here would still
// cost the file its filter.
const fileTimeFormat = time.RFC3339

// Build assembles the process logger from o. See BuildWriter for how the sinks
// are chosen. It returns the logger and the parsed level; an unparseable level
// is an error.
//
// The rotating file sink BuildWriter may open is not closeable through this
// entry point — for a process that configures its logger once and keeps it for
// its lifetime that is exactly right. A caller that reconfigures logging while
// running must use BuildWriter and close what the previous configuration left
// open, or it leaks a file descriptor per reconfiguration.
func Build(o Options, consoleOut, jsonOut, ring io.Writer, tees ...io.Writer) (zerolog.Logger, zerolog.Level, error) {
	w, _, lvl, err := BuildWriter(o, consoleOut, jsonOut, ring, tees...)
	if err != nil {
		return zerolog.Logger{}, 0, err
	}
	return zerolog.New(w).With().Timestamp().Logger(), lvl, nil
}

// BuildWriter assembles the log SINK from o, without binding it to a logger.
// The base sink is chosen by format: "json" writes structured records to
// jsonOut (stdout in production — the collector-friendly default for
// containers), any other value writes human-readable output to consoleOut
// (stderr). When o.File is set, a rotating file sink is added (app-managed
// rotation, for non-container deployments), carrying the same shape as the base
// — human-readable for "console", raw JSON for "json". ring, when non-nil, is
// always tee'd in so crash reports retain the recent-log tail.
//
// tees are extra writers added to the sink, for a consumer that is neither a
// sink of its own nor the crash ring — currently the log Tap that
// GET /api/v1/logs/events streams from. They are SEPARATE from ring because
// ring has one documented job (crash reports keep the recent tail) and
// overloading it would make that doc a half-truth.
//
// A tee receives the same raw JSON event every writer here does, so a consumer
// wanting human-readable output passes its own formatting wrapper — see
// Tap.Writer, which is exactly that case and explains why.
//
// It returns the sink, the rotator to close when this sink is replaced (nil
// when o.File is empty), and the parsed level; an unparseable level is an
// error.
//
// Split out from Build for callers that install ONE logger over a swappable
// writer and reconfigure by swapping the writer — the desktop app does this so
// a Settings change can apply live without writing zerolog's global log.Logger
// from an IPC goroutine while the rest of the process is logging through it.
func BuildWriter(o Options, consoleOut, jsonOut, ring io.Writer, tees ...io.Writer) (zerolog.LevelWriter, io.Closer, zerolog.Level, error) {
	level := o.Level
	if level == "" {
		level = "info"
	}
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		return nil, nil, 0, err
	}

	var base io.Writer
	if o.Format == "json" {
		base = jsonOut
	} else {
		base = zerolog.ConsoleWriter{Out: consoleOut}
	}

	writers := []io.Writer{base}
	var closer io.Closer
	if o.File != "" {
		rotator := &lumberjack.Logger{
			Filename:   o.File,
			MaxSize:    o.MaxSizeMB,
			MaxBackups: o.MaxBackups,
			MaxAge:     o.MaxAgeDays,
			Compress:   true,
		}
		closer = rotator
		// Match the file's shape to the configured format. Without this the
		// file is raw JSON even when the operator asked for console — which is
		// the only output a macOS bundle actually surfaces, since
		// LaunchServices points its stderr at /dev/null.
		//
		// NoColor: the ConsoleWriter colorises by default, and ANSI escapes in
		// a file are worse than the JSON they replace.
		//
		// TimeFormat: see fileTimeFormat. Deliberately set on the file writer
		// ONLY — the stderr writer above keeps ConsoleWriter's short
		// time.Kitchen default, which is what a developer watching a terminal
		// wants and where the date is never in doubt anyway.
		if o.Format == "json" {
			writers = append(writers, rotator)
		} else {
			writers = append(writers, zerolog.ConsoleWriter{
				Out: rotator, NoColor: true, TimeFormat: fileTimeFormat,
			})
		}
	}
	if ring != nil {
		writers = append(writers, ring)
	}
	for _, tee := range tees {
		if tee != nil {
			writers = append(writers, tee)
		}
	}

	return zerolog.MultiLevelWriter(writers...), closer, lvl, nil
}
