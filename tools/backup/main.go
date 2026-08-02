// knomit-backup is knomit's replication agent: the child process that runs
// litestream on knomit's behalf.
//
// It is not meant to be run by hand. knomit spawns it, speaks newline-delimited
// JSON to its stdin/stdout (see internal/backup/proto), and reads its stderr into
// the server log. It exits when its stdin reaches EOF, which is also what
// happens when knomit dies — including under SIGKILL, where no shutdown handler
// of knomit's could run. That is deliberate: an orphaned replication agent
// writing to the same object-store prefix as a restarted knomit is the two-
// writers case the whole feature exists to detect and refuse.
//
// # Why this binary exists at all
//
// litestream v0.5 drives SQLite through modernc.org/sqlite; knomit drives the
// same files through cgo mattn/go-sqlite3 and cannot switch, because sqlite-vec
// has no modernc build. Two SQLite BUILDS inside one process do not see each
// other's locks — POSIX advisory record locks do not conflict between
// descriptors held by the same process, and SQLite's compensating per-process
// inode table belongs to one build. Demonstrated, not theorised: knomit's close
// deleted litestream's -wal while litestream held a read lock, and removed the
// -shm while it was mapped, 3 runs out of 3. Across PROCESSES those locks work
// exactly as SQLite intends, which is why litestream lives here now.
//
// # stdout is protocol, stderr is logs
//
// A single stray fmt.Println anywhere in this process — ours, or a dependency's
// — would corrupt the channel. main therefore takes the real stdout for the
// protocol and REPLACES os.Stdout with stderr before anything else runs, so a
// stray print lands harmlessly in the log rather than mid-line on the wire.
// The standard log package and slog's default are pointed at stderr for the
// same reason.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"

	"knomit/internal/version"
	"knomit/tools/backup/agent"
)

func main() {
	// FIRST, before any other code can write: move the protocol pipe to a
	// private file descriptor and put stderr on fd 1. Two guards, because they
	// cover different things:
	//
	//   - fd 1 IS stderr afterwards, so anything holding the NUMBER 1 — a
	//     package-level variable initialised before main, C stdio, a future
	//     dependency — writes to the log. litestream.LogWriter is precisely
	//     such a variable; it is dormant in v0.5.15, and this makes an upgrade
	//     that starts using it harmless rather than corrupting.
	//   - os.Stdout points at stderr, so Go code resolving it at call time
	//     (fmt.Println and friends) goes there too.
	//
	// A failure here is fatal on purpose: continuing would mean serving the
	// protocol on a stream something else might also write to.
	protocol, err := claimProtocolStream()
	if err != nil {
		fmt.Fprintf(os.Stderr, "knomit-backup: %v\n", err)
		os.Exit(1)
	}
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)
	flag.CommandLine.SetOutput(os.Stderr)

	logLevel := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		debug       = flag.Bool("debug", false, "log at debug level")
	)
	flag.Parse()
	if *debug {
		logLevel.Set(slog.LevelDebug)
	}
	if *showVersion {
		fmt.Fprintln(os.Stderr, version.String())
		return
	}

	// Not named `agent`: that would shadow the package it comes from.
	ag := agent.New(logger)
	if err := ag.Serve(context.Background(), os.Stdin, protocol); err != nil {
		logger.Error("backup agent stopped", "err", err.Error())
		os.Exit(1)
	}
}
