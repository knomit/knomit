package crashdump

import (
	"fmt"
	"runtime/debug"
)

// Guard is a deferred recover that, on panic, writes a crash bundle attributed
// to component and then re-panics so the runtime's GOTRACEBACK=crash handling
// still fires (full goroutine dump to stderr + optional core). Install it at
// the top of every long-lived goroutine and at main:
//
//	defer reporter.Guard("sync-loop")
//
// On a clean return it does nothing. A failure to write the bundle is
// deliberately swallowed (best-effort) — losing the report must never suppress
// the original crash.
func (r *Reporter) Guard(component string) {
	rec := recover()
	if rec == nil {
		return
	}
	stack := debug.Stack()
	_, _ = r.Write(component, fmt.Sprint(rec), stack)
	panic(rec)
}
