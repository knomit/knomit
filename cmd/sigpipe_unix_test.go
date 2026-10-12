//go:build unix

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A supervised server's stderr is a pipe its desktop reads. When the desktop
// dies, the next log line would end the server with SIGPIPE (the Go runtime's
// rule for fd 2), before its stdin-EOF shutdown removes server.json. The
// helper below writes to a stderr whose reader is gone and then records that
// it survived; with survivePipedStderrLoss it does, without it (the positive
// control, run in the same test) it is killed by SIGPIPE.
func TestSurvivePipedStderrLoss_AWriteToADeadStderrDoesNotKill(t *testing.T) {
	for _, c := range []struct {
		name    string
		protect string
		survive bool
	}{
		{"protected", "1", true},
		{"unprotected (positive control)", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "survived")
			cmd := exec.Command(os.Args[0], "-test.run=^TestSIGPIPEHelper$")
			cmd.Env = append(os.Environ(), "KNOMIT_SIGPIPE_HELPER="+marker, "KNOMIT_SIGPIPE_PROTECT="+c.protect)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = stderr.Close() // the "desktop" is gone: no reader left
			err = cmd.Wait()
			_, statErr := os.Stat(marker)
			survived := statErr == nil
			if survived != c.survive {
				t.Fatalf("survived=%v, want %v (wait: %v)", survived, c.survive, err)
			}
			if !c.survive && (err == nil || !strings.Contains(err.Error(), "broken pipe")) {
				t.Fatalf("the unprotected helper ended with %v, want death by SIGPIPE", err)
			}
		})
	}
}

// TestSIGPIPEHelper is the child process of the test above.
func TestSIGPIPEHelper(t *testing.T) {
	marker := os.Getenv("KNOMIT_SIGPIPE_HELPER")
	if marker == "" {
		t.Skip("helper process only")
	}
	if os.Getenv("KNOMIT_SIGPIPE_PROTECT") != "" {
		survivePipedStderrLoss()
	}
	time.Sleep(100 * time.Millisecond) // let the parent close its end
	for i := 0; i < 3; i++ {
		_, _ = os.Stderr.WriteString("supervisor closed stdin; shutting down\n")
	}
	_ = os.WriteFile(marker, []byte("ok"), 0o600)
	os.Exit(0)
}
