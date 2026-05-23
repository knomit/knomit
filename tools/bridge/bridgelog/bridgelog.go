// Package bridgelog wires zerolog to a rotating file shared by both the
// MCP stdio path and the claude hook subcommands, so a single tail -F
// reveals all bridge activity.
package bridgelog

import (
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultPath is used when no --log flag is supplied.
const DefaultPath = "/tmp/knomit-bridge.log"

// Init points the global zerolog logger at path with 4MB lumberjack rotation
// (3 backups, 7 days). KNOMIT_MCP_DEBUG=1 raises the level to Debug.
func Init(path string) {
	if path == "" {
		path = DefaultPath
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
