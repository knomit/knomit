// Package bridgelog wires zerolog to a rotating file shared by both the
// MCP stdio path and the claude hook subcommands, so a single tail -F
// reveals all bridge activity.
package bridgelog

import (
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultPath returns the log file used when no --log flag is supplied.
//
// A function, not a constant, because the constant was "/tmp/knomit-bridge.log"
// and Windows has no /tmp: that resolves to C:\tmp on the current drive, a
// directory that does not exist on a stock install and that an ordinary user
// may not be able to create. os.TempDir gives the per-user %TEMP% there and
// /tmp everywhere else, so the default lands somewhere writable on every
// platform.
func DefaultPath() string {
	return filepath.Join(os.TempDir(), "knomit-bridge.log")
}

// Init points the global zerolog logger at path with 4MB lumberjack rotation
// (3 backups, 7 days). KNOMIT_MCP_DEBUG=1 raises the level to Debug.
func Init(path string) {
	if path == "" {
		path = DefaultPath()
	}
	writer := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    4,
		MaxBackups: 3,
		MaxAge:     7,
		Compress:   false,
	}
	level := zerolog.InfoLevel
	if os.Getenv("KNOMIT_MCP_DEBUG") != "" {
		level = zerolog.DebugLevel
	}
	log.Logger = zerolog.New(writer).Level(level).With().Timestamp().Int("pid", os.Getpid()).Logger()
}
