package app

import (
	"fmt"
	"os"
	"testing"

	"knomit/internal/backuptest"
)

// backupAgentBin is the knomit-backup binary the backup-enabled tests in this
// package run against. Replication lives in a child process, so a test that
// boots with backup enabled needs that binary to exist.
var backupAgentBin string

// TestMain builds it from the working tree, so `go test ./...` needs no
// `make build` first and never runs against a stale artefact in dist/.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "knomit-backup-agent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "app tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	bin, err := backuptest.Build(dir)
	if err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "app tests: %v\n", err)
		os.Exit(1)
	}
	backupAgentBin = bin

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
