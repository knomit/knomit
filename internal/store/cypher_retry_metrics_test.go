package store

import (
	"errors"
	"testing"
)

func TestWithCypherRetry_CountsTransientRetries(t *testing.T) {
	before := cypherRetryTotal.Value()

	// Fail transiently twice, then succeed → 2 retries counted.
	attempts := 0
	err := withCypherRetry(func() error {
		attempts++
		if attempts <= 2 {
			return errors.New("no such column: _gql_default_alias_1.id")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withCypherRetry: %v", err)
	}

	if got := cypherRetryTotal.Value() - before; got != 2 {
		t.Errorf("cypher retries counted = %d, want 2", got)
	}
}

func TestWithCypherRetry_ExhaustionCountsRetriesNotAttempts(t *testing.T) {
	before := cypherRetryTotal.Value()

	// Every attempt collides transiently → retries are exhausted and the call
	// ultimately fails. The counter must record the number of retries actually
	// performed (maxAttempts-1), not one per attempt: the final attempt is not
	// followed by a retry, so counting it would over-count by one.
	attempts := 0
	err := withCypherRetry(func() error {
		attempts++
		return errors.New("no such column: _gql_default_alias_1.id")
	})
	if err == nil {
		t.Fatal("withCypherRetry: expected the exhausted transient error")
	}
	if attempts != 5 {
		t.Fatalf("fn ran %d times, want 5 (maxAttempts)", attempts)
	}
	if got := cypherRetryTotal.Value() - before; got != 4 {
		t.Errorf("retries counted on exhaustion = %d, want 4 (one per retry, not per attempt)", got)
	}
}

func TestWithCypherRetry_NonTransientNotCounted(t *testing.T) {
	before := cypherRetryTotal.Value()
	_ = withCypherRetry(func() error { return errors.New("syntax error") })
	if got := cypherRetryTotal.Value() - before; got != 0 {
		t.Errorf("non-transient error counted as retry: %d", got)
	}
}
