package diag

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func serve(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestLogLevel_GetReportsCurrent(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	rr := serve(t, NewServer(Options{}), "GET", "/runtime/loglevel")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "info") {
		t.Errorf("body %q does not report current level", rr.Body.String())
	}
}

func TestLogLevel_PostSetsLevel(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	defer zerolog.SetGlobalLevel(zerolog.InfoLevel)

	rr := serve(t, NewServer(Options{}), "POST", "/runtime/loglevel?level=debug")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if zerolog.GlobalLevel() != zerolog.DebugLevel {
		t.Errorf("global level = %v, want debug", zerolog.GlobalLevel())
	}
}

func TestLogLevel_PostRejectsUnknown(t *testing.T) {
	rr := serve(t, NewServer(Options{}), "POST", "/runtime/loglevel?level=loud")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestProfileMutex_Toggle(t *testing.T) {
	defer runtime.SetMutexProfileFraction(0)

	rr := serve(t, NewServer(Options{}), "POST", "/runtime/profile/mutex?rate=10")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := runtime.SetMutexProfileFraction(-1); got != 10 {
		t.Errorf("mutex profile fraction = %d, want 10", got)
	}
}

func TestProfileBlock_Toggle(t *testing.T) {
	defer runtime.SetBlockProfileRate(0)
	rr := serve(t, NewServer(Options{}), "POST", "/runtime/profile/block?rate=1000")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestGC_Triggers(t *testing.T) {
	before := numGC()
	rr := serve(t, NewServer(Options{}), "POST", "/runtime/gc")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if numGC() <= before {
		t.Errorf("GC count did not increase: before=%d after=%d", before, numGC())
	}
}

func TestStatus_ReportsRuntimeAndExtra(t *testing.T) {
	started := time.Now().Add(-90 * time.Second)
	s := NewServer(Options{
		StartedAt: started,
		StatusExtra: func() map[string]any {
			return map[string]any{"repos": []string{"core"}}
		},
	})
	rr := serve(t, s, "GET", "/runtime/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("status not JSON: %v\n%s", err, rr.Body.String())
	}
	if _, ok := got["uptime"]; !ok {
		t.Error("status missing uptime")
	}
	if _, ok := got["goroutines"]; !ok {
		t.Error("status missing goroutines")
	}
	if got["repos"] == nil {
		t.Error("status did not include injected extra (repos)")
	}
}

func numGC() uint32 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.NumGC
}
