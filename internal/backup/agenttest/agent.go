// Package agenttest builds the knomit-backup agent binary for tests.
//
// knomit's replication runs in a child process, so any test that exercises
// backup needs that binary to exist. Building it from TestMain — rather than
// expecting `make build` to have run — keeps `go test ./...` self-contained:
// a fresh checkout runs the whole suite with no setup step, and the binary
// under test is always the one built from the working tree rather than a stale
// artefact in dist/.
//
// It is a normal (non _test) package because more than one test package needs
// it: internal/backup and internal/app both boot a real agent. It imports
// nothing from testing, so it costs those packages nothing at build time.
package agenttest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// agentPkg is the package path of the agent binary's main.
const agentPkg = "./tools/backup"

var (
	once sync.Once
	path string
	err  error
)

// Build compiles the knomit-backup agent into destDir and returns its path.
//
// It builds at most ONCE per test process however many times it is called: the
// agent pulls in all of litestream, and paying for that link more than once per
// package would dominate the suite's runtime.
//
// CGO is switched OFF deliberately. The agent's dependencies — litestream and
// modernc.org/sqlite — are pure Go, and that is the entire point of the split:
// if this build ever starts needing cgo, something has pulled knomit's cgo
// SQLite back into the agent and the link that was severed has been restored.
func Build(destDir string) (string, error) {
	once.Do(func() { path, err = build(destDir) })
	return path, err
}

func build(destDir string) (string, error) {
	root, rerr := moduleRoot()
	if rerr != nil {
		return "", rerr
	}
	out := filepath.Join(destDir, "knomit-backup")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, agentPkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if combined, berr := cmd.CombinedOutput(); berr != nil {
		return "", fmt.Errorf("building %s: %w\n%s", agentPkg, berr, combined)
	}
	return out, nil
}

// moduleRoot locates the repository root from this file's own compiled-in
// path, so tests find it no matter which package directory they run in.
//
// It WALKS up looking for go.mod rather than counting parent directories. A
// fixed number of filepath.Dir calls encodes this package's depth in the tree,
// which is invisible until someone moves the package and every backup test
// fails with a path error that names neither the move nor the cause — as
// happened when this package moved from internal/backuptest to
// internal/backup/agenttest.
func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("agenttest: cannot determine this package's source path")
	}
	dir := filepath.Dir(file)
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("agenttest: no go.mod in any parent of %s", filepath.Dir(file))
		}
		dir = parent
	}
}
