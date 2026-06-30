package crashdump

// Global is the process-wide ring buffer that feeds the recent-log tail into
// crash reports. main installs it (and tees it into the zerolog writer) at the
// very start of the program, before config is loaded; serve reads it once the
// crashes directory is known and constructs the Reporter. It mirrors the
// codebase's existing use of a global zerolog logger. It may be nil in tests.
var Global *RingWriter
