package oauth

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// N2 (3a review): the CIMD fetch never goes through a proxy, whatever the
// environment says. R7 required a test per guard and `Proxy: nil` had none:
// the pinned DialContext makes a proxy unreachable anyway, so the sabotage
// "Proxy: http.ProxyFromEnvironment" does not reach the proxy — it sends
// CONNECT to the pinned metadata server instead, and the fetch FAILS. So this
// test asserts both halves: the fetch succeeds, and the proxy saw nothing.
//
// It runs the resolve in a re-executed test binary, because
// http.ProxyFromEnvironment reads the environment ONCE per process: a
// t.Setenv here could come after something else had already cached it, and
// the sabotage would then pass for the wrong reason.
//
// Sabotage (run 2026-09-24): Proxy: http.ProxyFromEnvironment in the fetch
// transport makes the child's resolve fail ("RESOLVE-ERR") and this fails.
func TestResolver_IgnoresProxyEnvironment(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	proxy := "http://" + ln.Addr().String()
	cmd := exec.Command(os.Args[0], "-test.run=^TestResolver_ProxyEnvironmentChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "KNOMIT_TEST_PROXY_CHILD=1",
		"HTTPS_PROXY="+proxy, "https_proxy="+proxy, "HTTP_PROXY="+proxy, "http_proxy="+proxy, "NO_PROXY=", "no_proxy=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "RESOLVED") {
		t.Fatalf("child: %v\n%s", err, out)
	}
	time.Sleep(50 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the proxy from the environment received %d connection(s)", n)
	}
}

// TestResolver_ProxyEnvironmentChild is the re-executed half; it does
// nothing in a normal run.
func TestResolver_ProxyEnvironmentChild(t *testing.T) {
	if os.Getenv("KNOMIT_TEST_PROXY_CHILD") != "1" {
		t.Skip("helper for TestResolver_IgnoresProxyEnvironment")
	}
	f := newCIMDFixture(t)
	r := newResolverFor(f.fetcher(), &clock{t: time.Unix(1_790_000_000, 0)})
	if _, err := r.Resolve(context.Background(), f.url); err != nil {
		fmt.Println("RESOLVE-ERR", err)
		t.Fatal(err)
	}
	fmt.Println("RESOLVED")
}
