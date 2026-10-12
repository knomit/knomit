//go:build desktop

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// killParentAndExpectChildGone starts a fake desktop (fakeParentMain), which
// starts a fake server in childMode the way runChild does; SIGKILLs the
// desktop (no cleanup of any kind runs); and asserts the server is gone within
// two seconds. Which mechanism ends it depends on childMode: "ready" honours
// stdin EOF (every OS), "ignore-stdin" does not, so only the platform backstop
// (Linux Pdeathsig, the Windows Job Object) can end it.
func killParentAndExpectChildGone(t *testing.T, childMode string) {
	t.Helper()
	e, _ := newFakeEnv(t, childMode)
	bin := filepath.Join(e.bundle, serverExecName+exeSuffix)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KNOMIT_FAKE_PARENT=1", "FAKE_BIN="+bin, "FAKE_HOME="+e.home)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	pidc := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "CHILD "); ok {
				pid, _ := strconv.Atoi(rest)
				pidc <- pid
				return
			}
		}
		pidc <- 0
	}()
	var child int
	select {
	case child = <-pidc:
	case <-time.After(20 * time.Second):
		t.Fatal("the fake desktop did not start its server")
	}
	if child == 0 {
		t.Fatal("the fake desktop failed to start its server")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(child); err == nil {
			_ = p.Kill()
		}
	})
	// The server must be up (listening, reading stdin) before the parent dies,
	// or the test would pass on a server that never started.
	eventually(t, 20*time.Second, "the server to answer on its socket", func() bool {
		probe := newSupervisor(supervisorConfig{socket: e.socket, up: &uiUpstream{}})
		_, err := probe.probe(t.Context())
		return err == nil
	})
	if !processAlive(child) {
		t.Fatal("positive control: the server is not running before the parent is killed")
	}

	if err := cmd.Process.Kill(); err != nil { // SIGKILL / TerminateProcess
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitGone(t, child, 2*time.Second)
}

// On every OS: a server that honours stdin EOF ends when its desktop is
// killed, because the kernel closes the desktop's end of the pipe.
func TestSupervisor_KilledDesktopEndsTheServerThroughStdin(t *testing.T) {
	killParentAndExpectChildGone(t, "ready")
}
