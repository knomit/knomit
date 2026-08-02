package storytests

import (
	"fmt"
	"os"
	"testing"

	"knomit/internal/backup/agenttest"
)

// backupAgentBin is the knomit-backup binary the backup story tests run
// against. Replication lives in a child process — knomit itself links no
// litestream — so a scenario that boots a replicating instance needs that
// binary to exist, and under `go test` there is no installed knomit executable
// for the sibling search to find it beside.
var backupAgentBin string

// TestMain builds it from the working tree, so `go test ./...` needs no
// `make build` first and never runs against a stale artefact in dist/. Same
// pattern as internal/backup and internal/app; agenttest.Build compiles at
// most once per process however many packages ask for it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "knomit-backup-agent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "storytests: temp dir: %v\n", err)
		os.Exit(1)
	}
	bin, err := agenttest.Build(dir)
	if err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "storytests: %v\n", err)
		os.Exit(1)
	}
	backupAgentBin = bin

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
