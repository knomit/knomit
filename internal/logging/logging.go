package logging

import (
	"io"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"

	"knomit/internal/config"
)

// fileTimeFormat is what the human-readable FILE sink stamps on each line.
//
// It exists because ConsoleWriter's default is time.Kitchen ("5:01PM"), which
// is right for a developer watching a terminal and useless in a file: with
// MaxAge measured in days and backups kept, a log full of bare clock times
// gives no way to tell today's lines from Monday's. The file is the ONLY log
// surface a macOS bundle has (LaunchServices points its stderr at /dev/null),
// so this is the timestamp most users will ever read.
//
// RFC3339 specifically, and not a "Jan 2 15:04:05" style: the Logs window
// takes the level to be the SECOND whitespace-separated token of a line (see
// ui/src/LogView.tsx levelOf), so a timestamp containing a space would shift
// the level to the third token and silently break the window's level filter.
// RFC3339 has no spaces. TestFileSinkTimestampIsDatedAndSpaceFree pins that.
const fileTimeFormat = time.RFC3339

// Build assembles the process logger from log configuration. See BuildWriter
// for how the sinks are chosen. It returns the logger and the parsed level; an
// unparseable level is an error.
//
// The rotating file sink BuildWriter may open is not closeable through this
// entry point — for a process that configures its logger once and keeps it for
// its lifetime that is exactly right. A caller that reconfigures logging while
// running must use BuildWriter and close what the previous configuration left
// open, or it leaks a file descriptor per reconfiguration.
func Build(lc config.LogConfig, consoleOut, jsonOut, ring io.Writer) (zerolog.Logger, zerolog.Level, error) {
	w, _, lvl, err := BuildWriter(lc, consoleOut, jsonOut, ring)
	if err != nil {
		return zerolog.Logger{}, 0, err
	}
	return zerolog.New(w).With().Timestamp().Logger(), lvl, nil
}

// BuildWriter assembles the log SINK from log configuration, without binding it
// to a logger. The base sink is chosen by format: "json" writes structured
// records to jsonOut (stdout in production — the collector-friendly default for
// containers), any other value writes human-readable output to consoleOut
// (stderr). When lc.File is set, a rotating file sink is added (app-managed
// rotation, for non-container deployments), carrying the same shape as the base
// — human-readable for "console", raw JSON for "json". ring, when non-nil, is
// always tee'd in so crash reports retain the recent-log tail.
//
// It returns the sink, the rotator to close when this sink is replaced (nil
// when lc.File is empty), and the parsed level; an unparseable level is an
// error.
//
// Split out from Build for callers that install ONE logger over a swappable
// writer and reconfigure by swapping the writer — the desktop app does this so
// a Settings change can apply live without writing zerolog's global log.Logger
// from an IPC goroutine while the rest of the process is logging through it.
func BuildWriter(lc config.LogConfig, consoleOut, jsonOut, ring io.Writer) (zerolog.LevelWriter, io.Closer, zerolog.Level, error) {
	level := lc.Level
	if level == "" {
		level = "info"
	}
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		return nil, nil, 0, err
	}

	var base io.Writer
	if lc.Format == "json" {
		base = jsonOut
	} else {
		base = zerolog.ConsoleWriter{Out: consoleOut}
	}

	writers := []io.Writer{base}
	var closer io.Closer
	if lc.File != "" {
		rotator := &lumberjack.Logger{
			Filename:   lc.File,
			MaxSize:    lc.MaxSizeMB,
			MaxBackups: lc.MaxBackups,
			MaxAge:     lc.MaxAgeDays,
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
		if lc.Format == "json" {
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

	return zerolog.MultiLevelWriter(writers...), closer, lvl, nil
}
