//go:build desktop

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/logging"
	"knomit/tools/desktop/internal/paths"
)

// Desktop rotation defaults, used when knomit.toml does not set them. Ten
// megabytes across three backups is a fortnight of ordinary desktop chatter
// without letting a log loop fill the disk.
const (
	defaultLogMaxSizeMB  = 10
	defaultLogMaxBackups = 3
	defaultLogMaxAgeDays = 14
)

// desktopLogConfig fills lc's gaps with desktop-appropriate defaults and
// returns the result. It NEVER overrides a value that is already set: an
// operator who wrote log.format = "json" into knomit.toml gets json.
//
// The desktop's defaults are deliberately not the server's. The server logs to
// stdout and treats a file as opt-in (a container's log driver owns rotation).
// A bundled macOS app has no usable stdout at all — LaunchServices points a
// bundle's stderr at /dev/null — so the file is the only surface a user has,
// which makes it mandatory here and makes "console" the right default format.
func desktopLogConfig(lc config.LogConfig, defaultFile string) config.LogConfig {
	if lc.Format == "" {
		lc.Format = "console"
	}
	// Mirrors logging.Build's own "" → info (internal/logging/logging.go:21-23),
	// so this is a no-op for the logger. It exists because the Settings dialog
	// SHOWS this value: a knomit.toml carrying a literal level = "" would
	// otherwise put an empty string in the form's level field, which
	// validateSettings then refuses on Save — a dialog the user cannot get out
	// of without hand-editing the file.
	if lc.Level == "" {
		lc.Level = "info"
	}
	if lc.File == "" {
		lc.File = defaultFile
	}
	if lc.MaxSizeMB == 0 {
		lc.MaxSizeMB = defaultLogMaxSizeMB
	}
	if lc.MaxBackups == 0 {
		lc.MaxBackups = defaultLogMaxBackups
	}
	if lc.MaxAgeDays == 0 {
		lc.MaxAgeDays = defaultLogMaxAgeDays
	}
	return lc
}

// logSink is the ONE writer the global logger is built on, and the only thing
// applyLogConfig mutates after startup.
//
// zerolog's `log.Logger` is a bare package-level var, not an atomic (see
// zerolog/log/log.go). Assigning it is safe exactly once, during
// single-threaded startup. It is NOT safe from SaveSettings, which arrives on a
// Wails IPC goroutine while the server is running and 60-odd packages under
// internal/ are logging through that same var — that is a data race with no
// symptom until it corrupts an interface word. (zerolog.SetGlobalLevel, right
// next to the assignment it used to sit above, IS atomic; the assignment was
// the unguarded half.)
//
// So the indirection: log.Logger is written once, in this file's init, and
// never again. A live level/format change swaps the writer underneath it
// instead — which the design commits to and the Settings form advertises, so
// "restart to change the format" was not an option.
type logSink struct {
	mu sync.RWMutex
	w  zerolog.LevelWriter
	// closer is the rotating file handle the CURRENT writer owns, if any. Held
	// so swap can close it: each applyLogConfig builds a fresh
	// lumberjack.Logger for the same path, and without this every Save would
	// leak one open fd for the life of the process.
	closer io.Closer
}

func newLogSink(w zerolog.LevelWriter) *logSink { return &logSink{w: w} }

// Write and WriteLevel hold the read lock for the whole write, not just to
// fetch the writer. That is what lets swap close the OUTGOING rotator safely:
// once swap's write lock is granted, no write is still in flight against it.
func (s *logSink) Write(p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.w.Write(p)
}

// WriteLevel keeps the sink a zerolog.LevelWriter. Without it zerolog wraps the
// sink in a levelWriterAdapter and the level never reaches the underlying
// MultiLevelWriter — harmless for today's sinks, a silent behaviour change for
// any level-aware one added later.
func (s *logSink) WriteLevel(l zerolog.Level, p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.w.WriteLevel(l, p)
}

// swap installs w, closing whatever rotator the previous writer owned.
func (s *logSink) swap(w zerolog.LevelWriter, closer io.Closer) {
	s.mu.Lock()
	old := s.closer
	s.w, s.closer = w, closer
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// globalSink backs log.Logger for the whole process. It starts on plain stderr
// so anything logged before bootstrapLogging runs still lands somewhere.
var globalSink = newLogSink(zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}))

// init points zerolog's global logger at globalSink. This is the ONLY
// assignment to log.Logger anywhere in the desktop app, and package
// initialisation is the only moment at which one is provably safe: the Go
// runtime runs it single-threaded, before main, before any goroutine exists.
//
// A sync.Once would NOT be equivalent. Once guarantees the write happens once;
// it guarantees nothing about concurrent READERS of log.Logger, so a lazily
// installed logger still races with anything already logging. (The race
// detector says so — this started life as an installGlobalLogger() call at the
// top of applyLogConfig, and TestApplyLogConfigIsSafeWhileLogging caught it.)
func init() {
	log.Logger = zerolog.New(globalSink).With().Timestamp().Logger()
}

// applyLogConfig builds the log sink from lc (after defaulting) and swaps it in
// under globalSink. Called at least twice per launch — once with a zero config
// to bootstrap, again once knomit.toml has been read — and then once per
// Settings save, from a Wails IPC goroutine. Safe from any goroutine.
func applyLogConfig(lc config.LogConfig, defaultFile string) error {
	resolved := desktopLogConfig(lc, defaultFile)
	w, closer, lvl, err := logging.BuildWriter(resolved, os.Stderr, os.Stderr, nil)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	globalSink.swap(w, closer)
	// Atomic in zerolog, and set AFTER the writer so a level widening can never
	// let a record through to a half-installed sink.
	zerolog.SetGlobalLevel(lvl)
	return nil
}

// bootstrapLogging installs a logger before anything else runs, so that a
// failure in config.Load itself still lands somewhere visible. It returns the
// log file path, or "" if the logs dir could not be resolved (in which case
// logging falls back to stderr alone).
//
// This is phase ONE of two. Phase two (applyLogConfig from run(), once
// knomit.toml has been read) is what honours the user's level and format. The
// split exists because config.Load can fail, and a config error logged nowhere
// is the single worst startup failure to debug.
func bootstrapLogging() string {
	dir, err := paths.LogsDir()
	if err != nil {
		return ""
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return ""
	}
	path := defaultLogPath()
	if applyErr := applyLogConfig(config.LogConfig{Level: "info"}, path); applyErr != nil {
		return ""
	}
	return path
}

// defaultLogPath returns the desktop's default log file path — the one used
// when knomit.toml does not name one. Separate from bootstrapLogging so phase
// two can resolve the same default without repeating the directory handling.
// Empty when the logs directory cannot be resolved at all.
func defaultLogPath() string {
	dir, err := paths.LogsDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "knomit-desktop.log")
}

// resolveLogFile returns the file the process will ACTUALLY be logging to: the
// configured [log] file when there is one, the desktop default otherwise.
//
// This must be the single source of that answer. Everything downstream of it —
// the logger's own sink, the Logs window's tailer, "Reveal in Finder", and the
// path the Settings dialog displays — has to agree, and it used to be resolved
// three separate ways. The version that ignored lc.File meant that setting
// `[log] file` (or exporting KNOMIT_LOG_FILE) left the Logs window tailing a
// file nothing was writing to: permanently blank, with no error anywhere, while
// the Settings dialog beside it named the correct path.
//
// It returns "" only when there is no log file at all — no configured path AND
// no resolvable logs directory. See startLogStream for what the Logs window
// does with that.
func resolveLogFile(cfg config.Config) string {
	return desktopLogConfig(cfg.Log, defaultLogPath()).File
}
