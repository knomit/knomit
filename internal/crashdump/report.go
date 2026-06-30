package crashdump

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"time"
)

// Report is the JSON crash bundle written when the process panics. It is the
// single artifact that answers "what was it doing when it died" — the faulting
// stack, every goroutine, memory state, build provenance, and the tail of the
// log that led up to the failure.
type Report struct {
	Timestamp     string           `json:"timestamp"`
	Component     string           `json:"component"`
	Cause         string           `json:"cause"`
	Stack         string           `json:"stack"`          // faulting goroutine
	GoroutineDump string           `json:"goroutine_dump"` // all goroutines
	MemStats      runtime.MemStats `json:"mem_stats"`
	BuildInfo     BuildInfo        `json:"build_info"`
	RecentLogs    []string         `json:"recent_logs"`
}

// BuildInfo captures the embedded VCS provenance (commit, dirty, Go version)
// that the Go toolchain records automatically — no -ldflags required.
type BuildInfo struct {
	GoVersion string            `json:"go_version"`
	Path      string            `json:"path"`
	Settings  map[string]string `json:"settings,omitempty"`
}

// Reporter writes crash bundles into a directory, drawing the recent-log tail
// from a RingWriter shared with the logger.
type Reporter struct {
	dir  string
	ring *RingWriter
}

// New returns a Reporter that writes bundles under dir. ring may be nil (no
// recent-log tail is captured in that case).
func New(dir string, ring *RingWriter) *Reporter {
	return &Reporter{dir: dir, ring: ring}
}

// Write builds and persists a crash bundle for component, attributing it to
// cause (typically the recovered panic value) with stack as the faulting
// goroutine's trace. It returns the path written. The crashes directory is
// created on demand so a crash is never lost to a missing directory.
func (r *Reporter) Write(component, cause string, stack []byte) (string, error) {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return "", fmt.Errorf("create crash dir: %w", err)
	}

	rep := r.build(component, cause, stack)
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal crash report: %w", err)
	}

	name := fmt.Sprintf("%s-%s.json", rep.Timestamp, sanitize(component))
	path := filepath.Join(r.dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", fmt.Errorf("write crash report: %w", err)
	}
	return path, nil
}

func (r *Reporter) build(component, cause string, stack []byte) Report {
	rep := Report{
		Timestamp: time.Now().UTC().Format("20060102T150405.000Z"),
		Component: component,
		Cause:     cause,
		Stack:     string(stack),
		BuildInfo: readBuildInfo(),
	}
	rep.GoroutineDump = allGoroutines()
	runtime.ReadMemStats(&rep.MemStats)
	if r.ring != nil {
		rep.RecentLogs = r.ring.Lines()
	}
	return rep
}

// writeAllGoroutines writes the stack traces of every goroutine (pprof debug=2
// format — the same dump GOTRACEBACK=crash prints to stderr) to w. It is the
// single place that decides how an all-goroutine dump is captured, shared by
// the in-memory crash report (allGoroutines) and the on-demand file dump
// (DumpGoroutines).
func writeAllGoroutines(w io.Writer) error {
	p := pprof.Lookup("goroutine")
	if p == nil {
		return nil
	}
	return p.WriteTo(w, 2)
}

// allGoroutines returns the stack traces of every goroutine as a string.
func allGoroutines() string {
	var sb strings.Builder
	_ = writeAllGoroutines(&sb)
	return sb.String()
}

func readBuildInfo() BuildInfo {
	bi := BuildInfo{}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return bi
	}
	bi.GoVersion = info.GoVersion
	bi.Path = info.Path
	bi.Settings = make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		// Keep the VCS-relevant settings; these are what identify the build.
		switch s.Key {
		case "vcs", "vcs.revision", "vcs.time", "vcs.modified", "GOARCH", "GOOS":
			bi.Settings[s.Key] = s.Value
		}
	}
	return bi
}

// sanitize makes a component name safe for use in a filename.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
