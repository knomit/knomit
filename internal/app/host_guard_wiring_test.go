package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/test/testenv"
)

// #281, test 6: the bind host is a loopback Host, and it is the EFFECTIVE
// bind host — knomit.toml, KNOMIT_HOST, or `knomit serve --host`, which
// cmd/serve.go applies to cfg.Host AFTER config.Load and before app.New.
// Every case boots through New, because the derivation point is what is
// under test: computing the set at config.Load (sabotage S9) passes the
// TOML and env cases and fails the --host one, which is why it exists.
// Dropping the auto-include altogether (S8) fails all three.
func TestApp_BindHostIsALoopbackHost(t *testing.T) {
	status := func(t *testing.T, cfg config.Config, host string) int {
		t.Helper()
		a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		defer a.Close()
		req := httptest.NewRequest("GET", "/api/v1/repos", nil)
		req.RemoteAddr = "127.0.0.1:1"
		req.Host = host
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	load := func(t *testing.T, toml string) config.Config {
		t.Helper()
		home := t.TempDir()
		t.Setenv("KNOMIT_HOME", home)
		if toml != "" {
			if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("toml", func(t *testing.T) {
		t.Setenv("KNOMIT_HOST", "")
		cfg := load(t, "host = \"mybox\"\n")
		if got := status(t, cfg, "mybox:19278"); got != http.StatusOK {
			t.Fatalf("host = mybox in knomit.toml: %d", got)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("KNOMIT_HOST", "mybox")
		cfg := load(t, "")
		if got := status(t, cfg, "mybox:19278"); got != http.StatusOK {
			t.Fatalf("KNOMIT_HOST=mybox: %d", got)
		}
	})
	t.Run("flag", func(t *testing.T) {
		t.Setenv("KNOMIT_HOST", "")
		cfg := load(t, "host = \"localhost\"\n")
		if got := status(t, cfg, "mybox:19278"); got != http.StatusMisdirectedRequest {
			t.Fatalf("without --host, mybox must be refused: %d", got)
		}
		cfg.Host = "mybox" // what `knomit serve --host mybox` does, after Load
		if got := status(t, cfg, "mybox:19278"); got != http.StatusOK {
			t.Fatalf("--host mybox: %d", got)
		}
	})
	t.Run("wildcard adds no name", func(t *testing.T) {
		t.Setenv("KNOMIT_HOST", "0.0.0.0")
		cfg := load(t, "")
		if got := status(t, cfg, "mybox:19278"); got != http.StatusMisdirectedRequest {
			t.Fatalf("0.0.0.0 bind: %d", got)
		}
	})
	t.Run("loopback_hosts", func(t *testing.T) {
		t.Setenv("KNOMIT_HOST", "")
		cfg := load(t, "[auth]\nloopback_hosts = [\"Box.Tail1234.ts.net\"]\n")
		if got := status(t, cfg, "box.tail1234.ts.net"); got != http.StatusOK {
			t.Fatalf("listed name: %d", got)
		}
	})
}

// #287: the Origin guard reaches both boot paths. `knomit serve` and the
// desktop app each build their server through New and serve Server.Handler;
// the desktop passes its Wails origins as Options.CORSOrigins
// (tools/desktop/app.go desktopAppOptions, pinned by its app_options_test).
// Booting through New is what is under test: an allowlist threaded into the
// CORS middleware but not into AuthMiddleware lets the webview preflight and
// then refuses its write.
func TestApp_OriginGuardWiring(t *testing.T) {
	wails := []string{"wails://localhost", "http://wails.localhost"}
	post := func(t *testing.T, opts Options, origin string) (int, string) {
		t.Helper()
		t.Setenv("KNOMIT_HOME", t.TempDir())
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		opts.Embedder = &testenv.DeterministicEmbedder{}
		a, err := New(context.Background(), cfg, opts)
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		defer a.Close()
		req := httptest.NewRequest("POST", "/api/v1/ontologies:validate", strings.NewReader("topics: {}\n"))
		req.RemoteAddr = "127.0.0.1:1"
		req.Host = "127.0.0.1:19278"
		req.Header.Set("Content-Type", "text/yaml")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}
	refused := func(code int, body string) bool {
		return code == http.StatusForbidden && strings.Contains(body, "Cross-origin request refused")
	}

	serve := Options{APIOnly: true}
	desktop := Options{APIOnly: true, CORSOrigins: wails}

	if code, body := post(t, serve, "https://evil.example"); !refused(code, body) {
		t.Fatalf("serve, foreign Origin: %d %.200s; want 403 Cross-origin request refused", code, body)
	}
	if code, body := post(t, desktop, "https://evil.example"); !refused(code, body) {
		t.Fatalf("desktop, foreign Origin: %d %.200s; want 403 Cross-origin request refused", code, body)
	}
	if code, body := post(t, serve, "wails://localhost"); !refused(code, body) {
		t.Fatalf("serve, Wails Origin (not allowlisted there): %d %.200s; want 403", code, body)
	}
	for _, origin := range wails {
		if code, body := post(t, desktop, origin); code >= 400 {
			t.Fatalf("desktop, Origin %s: %d %.200s; want the write served", origin, code, body)
		}
	}
	if code, body := post(t, serve, "http://127.0.0.1:19278"); code >= 400 {
		t.Fatalf("serve, own Origin: %d %.200s; want served", code, body)
	}
	if code, body := post(t, serve, ""); code >= 400 {
		t.Fatalf("serve, no Origin: %d %.200s; want served", code, body)
	}
}
