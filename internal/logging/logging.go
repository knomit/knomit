package logging

import (
	"io"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"

	"knomit/internal/config"
)

// Build assembles the process logger from log configuration. The base
// sink is chosen by format: "json" writes structured records to jsonOut
// (stdout in production — the collector-friendly default for containers), any
// other value writes human-readable output to consoleOut (stderr). When
// lc.File is set, a rotating JSON file sink is added (app-managed rotation, for
// non-container deployments). ring, when non-nil, is always tee'd in so crash
// reports retain the recent-log tail. It returns the logger and the parsed
// level; an unparseable level is an error.
func Build(lc config.LogConfig, consoleOut, jsonOut, ring io.Writer) (zerolog.Logger, zerolog.Level, error) {
	level := lc.Level
	if level == "" {
		level = "info"
	}
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		return zerolog.Logger{}, 0, err
	}

	var base io.Writer
	if lc.Format == "json" {
		base = jsonOut
	} else {
		base = zerolog.ConsoleWriter{Out: consoleOut}
	}

	writers := []io.Writer{base}
	if lc.File != "" {
		rotator := &lumberjack.Logger{
			Filename:   lc.File,
			MaxSize:    lc.MaxSizeMB,
			MaxBackups: lc.MaxBackups,
			MaxAge:     lc.MaxAgeDays,
			Compress:   true,
		}
		// Match the file's shape to the configured format. Without this the
		// file is raw JSON even when the operator asked for console — which is
		// the only output a macOS bundle actually surfaces, since
		// LaunchServices points its stderr at /dev/null.
		//
		// NoColor: the ConsoleWriter colorises by default, and ANSI escapes in
		// a file are worse than the JSON they replace.
		if lc.Format == "json" {
			writers = append(writers, rotator)
		} else {
			writers = append(writers, zerolog.ConsoleWriter{Out: rotator, NoColor: true})
		}
	}
	if ring != nil {
		writers = append(writers, ring)
	}

	w := zerolog.MultiLevelWriter(writers...)
	return zerolog.New(w).With().Timestamp().Logger(), lvl, nil
}
