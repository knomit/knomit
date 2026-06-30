package crashdump

import (
	"fmt"
	"runtime/debug"
	"sync/atomic"
)

// Global is the process-wide ring buffer that feeds the recent-log tail into
// crash reports. main installs it (and tees it into the zerolog writer) at the
// very start of the program, before config is loaded; serve reads it once the
// crashes directory is known and constructs the Reporter. It mirrors the
// codebase's existing use of a global zerolog logger. It may be nil in tests.
var Global *RingWriter

// globalReporter is the process-wide Reporter that recovered-panic sites
// (chi Recoverer, TaskHub) report through, so a panic that is RECOVERED (and
// thus never reaches Guard) still leaves a forensic bundle. Set by serve.
var globalReporter atomic.Pointer[Reporter]

// SetGlobalReporter installs (or, with nil, clears) the process-wide Reporter
// used by ReportRecovered.
func SetGlobalReporter(r *Reporter) { globalReporter.Store(r) }

// ReportRecovered writes a crash bundle for a recovered panic attributed to
// component, capturing the current goroutine stack (which, inside a deferred
// recover, still holds the panicking frames). It is a no-op when no global
// Reporter is installed, and never itself panics — losing the bundle must not
// disturb the recovery path.
func ReportRecovered(component string, cause any) {
	r := globalReporter.Load()
	if r == nil {
		return
	}
	_, _ = r.Write(component, fmt.Sprint(cause), debug.Stack())
}
