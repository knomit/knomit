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

func TestWithCypherRetry_NonTransientNotCounted(t *testing.T) {
	before := cypherRetryTotal.Value()
	_ = withCypherRetry(func() error { return errors.New("syntax error") })
	if got := cypherRetryTotal.Value() - before; got != 0 {
		t.Errorf("non-transient error counted as retry: %d", got)
	}
}
