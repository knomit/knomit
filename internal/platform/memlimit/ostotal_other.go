//go:build !linux && !darwin && !windows

package memlimit

// osTotal has no implementation on this platform. Detection then reports
// SourceNone and the caller falls back to a fixed default — never an error,
// because embeddings are mandatory and must not be blocked by an unknown
// memory ceiling.
func osTotal() (int64, error) { return 0, errNotFound }
