package app

import (
	"knomit/internal/config"
	"knomit/internal/platform/logging"
)

// LoggingOptions translates the operator-facing log configuration into the
// struct the log sink actually consumes.
//
// It lives here because neither of the two packages it names can hold it.
// internal/platform/logging must not import internal/config — nothing under
// internal/platform may know about knomit, which is the property
// TestPlatformKnowsNothingAboutKnomit pins and the whole reason the tier
// exists. The mirror image is worse: a method on config.LogConfig would make
// internal/config import platform/logging and so gain lumberjack, compress/gzip,
// compress/flate and hash/crc32, and config is imported almost everywhere.
//
// internal/app is the composition tier — it already imports config, it already
// pulls the whole server closure, and it is already imported by both binaries
// that build a logger (cmd/serve.go and tools/desktop/app.go). Adding the edge
// here costs nothing that is not already paid.
//
// Being the SINGLE converter is the point. When the two call sites each spelled
// out the fields, a rotation key added to config.LogConfig and to logging.Options
// but wired at only one of them took effect in one binary and vanished in the
// other — and only one of those call sites is even compiled by default, since
// tools/desktop sits behind the `desktop` build tag. Now there is one place to
// forget, and it is compiled into everything.
//
// The mapping is deliberately not exhaustive over config.LogConfig.
// SlowRequestMS and CrashFile live in that struct too and are read by the HTTP
// middleware and by crashdump respectively — never by the logger. Adding them
// here would not wire them up, it would just imply they were.
func LoggingOptions(lc config.LogConfig) logging.Options {
	return logging.Options{
		Format:     lc.Format,
		Level:      lc.Level,
		File:       lc.File,
		MaxSizeMB:  lc.MaxSizeMB,
		MaxBackups: lc.MaxBackups,
		MaxAgeDays: lc.MaxAgeDays,
	}
}
