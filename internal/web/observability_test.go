package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/obs/metrics"
	"knomit/internal/obs/reqinfo"
)

// captureLogs redirects the global zerolog logger into a buffer for the
// duration of the test and returns it.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

// slowEntry finds the single "slow request" line in captured log output and
// decodes it. Fails the test if there isn't exactly one.
func slowEntry(t *testing.T, out string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if entry["message"] == "slow request" {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 slow-request log, got %d:\n%s", len(found), out)
	}
	return found[0]
}

func TestMetricsMiddleware_CountsPanicAs500(t *testing.T) {
	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)      // outer: catches the re-panic, renders 500
	r.Use(metricsMiddleware(reg, 0)) // inner: must still count the request
	r.Get("/boom/{id}", func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/boom/42", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}

	var sb strings.Builder
	reg.WriteProm(&sb)
	out := sb.String()
	// Labelled by route PATTERN and attributed to the 500 the Recoverer sent —
	// a panicking handler must not vanish from request metrics.
	want := `knomit_http_requests_total{route="/boom/{id}",method="GET",status="500"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestMetricsMiddleware_CountsByRoutePattern(t *testing.T) {
	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 0))
	r.Get("/repos/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Two requests to the SAME pattern but different concrete paths must
	// collapse into one series (bounded cardinality).
	for _, p := range []string{"/repos/core", "/repos/other"} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
	}

	var sb strings.Builder
	reg.WriteProm(&sb)
	out := sb.String()
	want := `knomit_http_requests_total{route="/repos/{repo}",method="GET",status="200"} 2`
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestMetricsMiddleware_SlowRequestLogs(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 1)) // 1ms threshold
	r.Get("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/slow", nil))

	if !strings.Contains(buf.String(), "slow request") {
		t.Errorf("expected a slow-request warning, got:\n%s", buf.String())
	}
}

func TestMetricsMiddleware_SlowRequestCarriesRequestDetail(t *testing.T) {
	buf := captureLogs(t)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(metricsMiddleware(metrics.NewRegistry(), 1))
	r.Post("/repos/{repo}/branches/{branch}/mcp", func(w http.ResponseWriter, req *http.Request) {
		reqinfo.FromContext(req.Context()).SetTool("knomit_query")
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/repos/core/branches/main/mcp?profile=code", nil)
	req.RemoteAddr = "10.1.2.3:54321"
	req.Header.Set("User-Agent", "claude-code/1.2.3")
	req.Header.Set("Mcp-Session-Id", "sess-abc")
	r.ServeHTTP(httptest.NewRecorder(), req)

	entry := slowEntry(t, buf.String())
	for field, want := range map[string]string{
		"route":       "/repos/{repo}/branches/{branch}/mcp",
		"method":      "POST",
		"path":        "/repos/core/branches/main/mcp",
		"query":       "profile=code",
		"repo":        "core",
		"branch":      "main",
		"remote":      "10.1.2.3:54321",
		"ua":          "claude-code/1.2.3",
		"mcp_session": "sess-abc",
		"mcp_tool":    "knomit_query",
	} {
		if got, _ := entry[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
	// RequestID is registered ahead of the middleware, so the correlation id
	// must survive into the warning.
	if got, _ := entry["req_id"].(string); got == "" {
		t.Errorf("req_id missing from slow-request log:\n%s", buf.String())
	}
}

// A slow request that has no repo/branch/lens, no query, no MCP session and no
// tool must not log those keys empty — a plain REST line stays short.
func TestMetricsMiddleware_SlowRequestOmitsAbsentFields(t *testing.T) {
	buf := captureLogs(t)

	r := chi.NewRouter()
	r.Use(metricsMiddleware(metrics.NewRegistry(), 1))
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/version", nil))

	entry := slowEntry(t, buf.String())
	for _, field := range []string{"repo", "branch", "lens", "query", "mcp_session", "mcp_tool", "req_id"} {
		if _, present := entry[field]; present {
			t.Errorf("field %q logged despite being empty:\n%s", field, buf.String())
		}
	}
	// The fields that are always available must still be there.
	if got, _ := entry["path"].(string); got != "/version" {
		t.Errorf("path = %q, want %q", got, "/version")
	}
}

// A query string is user-supplied and unbounded (knomit_query text, long filter
// lists). It is truncated so one pathological request cannot dominate the log.
func TestMetricsMiddleware_SlowRequestTruncatesQuery(t *testing.T) {
	buf := captureLogs(t)

	r := chi.NewRouter()
	r.Use(metricsMiddleware(metrics.NewRegistry(), 1))
	r.Get("/query", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	long := "text=" + strings.Repeat("x", 4096)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/query?"+long, nil))

	entry := slowEntry(t, buf.String())
	got, _ := entry["query"].(string)
	if len(got) > maxLoggedField+len(truncationSuffix) {
		t.Errorf("query field is %d bytes, want at most %d", len(got), maxLoggedField+len(truncationSuffix))
	}
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Errorf("truncated query not marked as truncated: %q", got)
	}
	if !strings.HasPrefix(got, "text=xxx") {
		t.Errorf("truncated query lost its head: %q", got)
	}
}

// Truncation must not slice a multi-byte rune in half — the result goes straight
// into a JSON log line.
func TestTruncateForLogKeepsValidUTF8(t *testing.T) {
	// "…" is 3 bytes, so a run of them crosses maxLoggedField mid-rune.
	got := truncateForLog(strings.Repeat("…", maxLoggedField))
	if !utf8.ValidString(got) {
		t.Errorf("truncated value is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Errorf("truncated value not marked: %q", got)
	}
	if len(got) > maxLoggedField+len(truncationSuffix) {
		t.Errorf("truncated value is %d bytes, want at most %d", len(got), maxLoggedField+len(truncationSuffix))
	}
}

// EVERY client-supplied field is capped, not just the query. Headers are bounded
// only by MaxHeaderBytes (1 MB), so an uncapped field lets one slow request emit
// a megabyte-scale log line — and repeating it fills the log.
func TestMetricsMiddleware_SlowRequestCapsEveryClientSuppliedField(t *testing.T) {
	buf := captureLogs(t)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(metricsMiddleware(metrics.NewRegistry(), 1))
	r.Get("/repos/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	huge := strings.Repeat("z", 8192)
	req := httptest.NewRequest("GET", "/repos/"+huge+"?text="+huge, nil)
	req.Header.Set("User-Agent", huge)
	req.Header.Set("X-Request-Id", huge) // chi's RequestID echoes this verbatim
	req.Header.Set("Mcp-Session-Id", huge)
	r.ServeHTTP(httptest.NewRecorder(), req)

	entry := slowEntry(t, buf.String())
	maxField := maxLoggedField + len(truncationSuffix)
	for _, field := range []string{"path", "query", "req_id", "ua", "mcp_session", "repo"} {
		got, _ := entry[field].(string)
		if len(got) > maxField {
			t.Errorf("%s is %d bytes, want at most %d", field, len(got), maxField)
		}
	}
}

// Truncation must lose only a trailing partial rune. Re-validating the whole
// prefix would discard everything before the first bad byte, gutting the field
// in exactly the malformed-request case worth inspecting.
func TestTruncateForLogKeepsThePrefixBeforeAnInvalidByte(t *testing.T) {
	// One invalid byte early in a long value: everything before it must survive.
	got := truncateForLog("a=0123456789\xffb=" + strings.Repeat("x", 4096))
	if !strings.HasPrefix(got, "a=0123456789") {
		t.Errorf("prefix before the invalid byte was discarded: %q", got)
	}
	if len(got) < maxLoggedField {
		t.Errorf("value truncated to %d bytes, want ~%d", len(got), maxLoggedField)
	}
}

// The middleware must survive being used outside a chi router — there is no
// route context to read a pattern or params from.
func TestMetricsMiddleware_WorksWithoutAChiRouter(t *testing.T) {
	buf := captureLogs(t)

	reg := metrics.NewRegistry()
	h := metricsMiddleware(reg, 1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	mux := http.NewServeMux()
	mux.Handle("/plain", h)
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/plain", nil))

	entry := slowEntry(t, buf.String())
	if got, _ := entry["route"].(string); got != "other" {
		t.Errorf("route = %q, want %q", got, "other")
	}
	var sb strings.Builder
	reg.WriteProm(&sb)
	if !strings.Contains(sb.String(), `knomit_http_requests_total{route="other",method="GET",status="200"} 1`) {
		t.Errorf("request not counted:\n%s", sb.String())
	}
}

// The annotation must reach handlers even when the slow threshold is disabled:
// SetTool is called unconditionally by the MCP layer and must never panic.
func TestMetricsMiddleware_AttachesReqInfoWhenSlowDisabled(t *testing.T) {
	r := chi.NewRouter()
	r.Use(metricsMiddleware(metrics.NewRegistry(), 0))
	var info *reqinfo.Info
	r.Get("/x", func(w http.ResponseWriter, req *http.Request) {
		info = reqinfo.FromContext(req.Context())
		info.SetTool("knomit_learn")
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	if info == nil {
		t.Fatal("no reqinfo.Info in the request context")
	}
	if got := info.Tool(); got != "knomit_learn" {
		t.Errorf("Tool() = %q, want %q", got, "knomit_learn")
	}
}

func TestMetricsMiddleware_SlowDisabledWhenZero(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 0)) // disabled
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))

	if strings.Contains(buf.String(), "slow request") {
		t.Errorf("slow-request log fired when threshold disabled:\n%s", buf.String())
	}
}

func TestMetricsMiddleware_StreamingNotLoggedAsSlow(t *testing.T) {
	var buf strings.Builder
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = orig }()

	reg := metrics.NewRegistry()
	r := chi.NewRouter()
	r.Use(metricsMiddleware(reg, 1)) // 1ms threshold
	// An SSE handler: sets text/event-stream and stays "open" past the
	// threshold. It must NOT be reported as a slow request.
	r.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(10 * time.Millisecond)
	})

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/stream", nil))

	if strings.Contains(buf.String(), "slow request") {
		t.Errorf("streaming (text/event-stream) response logged as slow:\n%s", buf.String())
	}
	// It must still be counted as a request, just not flagged slow.
	var sb strings.Builder
	reg.WriteProm(&sb)
	if !strings.Contains(sb.String(), `knomit_http_requests_total{route="/stream",method="GET",status="200"} 1`) {
		t.Errorf("streaming request was not counted:\n%s", sb.String())
	}
}
