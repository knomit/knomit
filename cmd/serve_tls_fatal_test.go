package cmd

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// knomit#256: `knomit serve` stays FATAL when [tls].addr is already bound,
// even though app.OpenTLSServer now tells that case apart
// (app.ErrTLSAddrInUse) so the desktop can survive it. log.Fatal exits the
// process, so the child half runs startTLSServer in a re-executed test binary
// and the parent asserts it died, naming the listener.
//
// The child's certificate is real and loadable, so the ONLY thing that can
// fail is the bind — a fixture that failed on the CRL would pass this test
// for the wrong reason.
//
// Sabotage: making startTLSServer skip errors.Is(err, app.ErrTLSAddrInUse)
// (warn and return nil, as the desktop does) turns this red.
func TestServeTLS_BindConflictIsFatal(t *testing.T) {
	if addr := os.Getenv("KNOMIT_TEST_TLS_FATAL_ADDR"); addr != "" {
		f := pkitest.New(t)
		self := f.Enroll(t, "server", pki.RoleInstance)
		dir := t.TempDir()
		f.Install(t, self, dir)
		startTLSServer(t.Context(), config.TLSConfig{Addr: addr, Dir: dir}, self.KeyPath, &http.Server{})
		t.Log("CHILD SURVIVED startTLSServer")
		return
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeTLS_BindConflictIsFatal$", "-test.v")
	cmd.Env = append(os.Environ(), "KNOMIT_TEST_TLS_FATAL_ADDR="+held.Addr().String())
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("serve survived a held [tls].addr (err %v):\n%s", err, out)
	}
	if strings.Contains(string(out), "CHILD SURVIVED") || !strings.Contains(string(out), "tls listener failed") ||
		!strings.Contains(string(out), "already in use") {
		t.Fatalf("the child did not die on the TLS bind:\n%s", out)
	}
}
