// The Wails event names shared with the Go side. Kept in a module of their own
// so each name is written exactly once on this side of the IPC: Go's spelling
// lives in tools/desktop/logstream.go and TestLogEventNameMatchesFrontend
// asserts the two agree, which it can only do while there is a single
// declaration here to read.
//
// Drift is silent — the window subscribes to a name nothing emits and simply
// stays empty — so the pinning test is the only thing that catches it.

/** A batch of already-formatted log lines from the desktop's log file. */
export const LOG_EVENT = 'desktop:log'
