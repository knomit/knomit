package supervisor_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"knomit/tools/tray/internal/supervisor"
)

// buildFakeServer compiles a tiny HTTP server that listens on --port for the
// duration of the test, then returns its path. Used instead of the real
// knomit binary to keep tests fast and self-contained.
func buildFakeServer(t *testing.T) string {
	t.Helper()
	// The supervisor always passes "serve" as the first argument (matching the
	// real knomit CLI).  We skip it here so that flag.Parse sees only flags.
	src := `package main
import (
	"flag"
	"fmt"
	"net/http"
	"os"
)
func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	port := flag.String("port", "", "")
	host := flag.String("host", "127.0.0.1", "")
	flag.CommandLine.Parse(args)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "fake ok")
	})
	http.ListenAndServe(*host+":"+*port, nil)
}
`
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write fake src: %v", err)
	}
	binPath := filepath.Join(dir, "fakeserve")
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v: %s", err, out)
	}
	return binPath
}

func TestSupervisor_StartsAndServes(t *testing.T) {
	bin := buildFakeServer(t)
	logDir := t.TempDir()

	s := supervisor.New(supervisor.Config{
		Binary:  bin,
		Port:    0, // supervisor will pick
		LogsDir: logDir,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(2 * time.Second)

	// Wait up to 2s for the server to accept a connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", s.URL()[len("http://"):], 100*time.Millisecond)
		if err == nil {
			c.Close()
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s", s.URL())
}

func TestSupervisor_OnStateChange_CanCallAccessors(t *testing.T) {
	bin := buildFakeServer(t)
	s := supervisor.New(supervisor.Config{
		Binary:  bin,
		Port:    0,
		LogsDir: t.TempDir(),
	})

	// Record port observed from within the callback. If the callback
	// held the supervisor's mutex, Port() would deadlock here.
	portCh := make(chan int, 4)
	s.OnStateChange(func(st supervisor.State) {
		// Port() acquires s.mu; must not deadlock.
		portCh <- s.Port()
	})

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop(2 * time.Second)

	select {
	case p := <-portCh:
		if p == 0 {
			t.Errorf("callback saw port 0; wanted > 0")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback never fired (likely deadlock)")
	}
}

func TestSupervisor_StopKillsChild(t *testing.T) {
	bin := buildFakeServer(t)
	s := supervisor.New(supervisor.Config{
		Binary:  bin,
		Port:    0,
		LogsDir: t.TempDir(),
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s.State() != supervisor.StateStopped {
		t.Errorf("state after Stop = %v, want Stopped", s.State())
	}
}
