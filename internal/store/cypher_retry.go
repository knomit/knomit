package store

import (
	"math/rand"
	"strings"
	"time"
)

// GraphQLite's cypher() UDF builds its SQL translation through a process-shared
// alias namespace (_gql_default_alias_N). It is NOT safe under concurrent
// cypher() calls: two reads translating at once on different pooled connections
// race and emit malformed SQL — surfaced as "no such column:
// _gql_default_alias_1.id" (raw, ≤0.3.10) or the same wrapped in GraphQLite's
// structured JSON ({"code":"EXECUTION_ERROR",...}, ≥0.4.1). The collision is
// non-deterministic (~0.1% under heavy concurrency) and clears on retry.
//
// Writes never hit this because they run inside _txlock=immediate transactions,
// which serialize cypher() execution via the SQLite write lock. Reads run in
// autocommit on the pinned pool and deliberately do NOT take the write lock
// (taking it would starve the pool against BEGIN IMMEDIATE waiters — the
// documented anti-pattern). So read collisions are handled here by retry
// instead of serialization.

// isTransientCypherError reports whether err is a concurrent-cypher failure
// that succeeds on retry.
//
// Two distinct transient signatures, same root cause (GraphQLite has no
// internal serialization or retry for concurrent cypher() execution):
//
//   - "_gql_default_alias…": the Cypher→SQL translation race on the
//     process-shared alias namespace, surfaced as malformed SQL ("no such
//     column: _gql_default_alias_N.id"), raw or wrapped in structured JSON.
//   - "abort due to ROLLBACK": GraphQLite aborts the in-flight statement
//     rather than blocking when a concurrent cypher writer/reader contends the
//     connection (seen from louvain() during cluster compute). The read
//     succeeds once the contender clears. ClusterFacts short-circuits the
//     *deterministic* empty-graph ROLLBACK before louvain runs, so a ROLLBACK
//     that reaches retry is the contention kind.
func isTransientCypherError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "_gql_default_alias") ||
		strings.Contains(msg, "abort due to ROLLBACK")
}

// withCypherRetry runs fn, retrying with jittered backoff while it returns the
// transient cypher race. fn must be idempotent (it re-runs the whole query and
// re-scans), as cypher reads are. Non-transient errors (and success) return
// immediately.
func withCypherRetry(fn func() error) error {
	const maxAttempts = 5
	var err error
	for attempt := range maxAttempts {
		if err = fn(); !isTransientCypherError(err) {
			return err
		}
		// Jittered sub-millisecond backoff to desync colliding goroutines so
		// they don't re-collide on the immediate retry.
		time.Sleep(time.Duration(attempt+1)*200*time.Microsecond +
			time.Duration(rand.Intn(300))*time.Microsecond)
	}
	return err
}
