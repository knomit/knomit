package backup

import (
	"fmt"
	"os"
	"testing"

	"knomit/internal/backuptest"
)

// agentBin is the knomit-backup binary these tests run against, built from the
// working tree by TestMain.
var agentBin string

// fakeAgentEnv makes the test binary re-enter itself as a SCRIPTED agent
// instead of running tests. It is how the protocol, supervision and failure
// paths are exercised against real pipes and a real child process without
// needing litestream: see fakeagent_test.go.
const fakeAgentEnv = "KNOMIT_TEST_FAKE_AGENT"

// TestMain builds the agent binary so `go test ./...` stays self-contained —
// no `make build` first, and the binary under test is always the one compiled
// from the working tree rather than a stale artefact in dist/.
//
// It also serves the other half of the fake-agent trick: when the marker
// environment variable is set, this process IS the agent, so it must speak the
// protocol and exit rather than run the suite.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeAgentEnv); mode != "" {
		runFakeAgent(mode)
		return
	}

	dir, err := os.MkdirTemp("", "knomit-backup-agent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup tests: temp dir: %v\n", err)
		os.Exit(1)
	}
	bin, err := backuptest.Build(dir)
	if err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "backup tests: %v\n", err)
		os.Exit(1)
	}
	agentBin = bin

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
