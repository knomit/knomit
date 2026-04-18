package store

// SetMaxPushAttemptsForTest overrides the push retry limit for tests.
// Returns a cleanup function that restores the original value; call via
// t.Cleanup or defer. Not part of the public API — only used to deterministically
// exercise the retry-exhaustion code path in concurrency tests.
func SetMaxPushAttemptsForTest(n int) func() {
	old := maxPushAttempts
	maxPushAttempts = n
	return func() { maxPushAttempts = old }
}
