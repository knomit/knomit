package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientCypherError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("disk full"), false},
		{"raw alias race", errors.New("SQL prepare failed: no such column: _gql_default_alias_1.id"), true},
		{"structured JSON alias race", errors.New(`{"error":"SQL prepare failed: no such column: _gql_default_alias_1.id","code":"EXECUTION_ERROR"}`), true},
		{"wrapped", fmt.Errorf("IncomingAtCommit: rows: %w", errors.New("no such column: _gql_default_alias_2.id")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientCypherError(c.err); got != c.want {
				t.Errorf("isTransientCypherError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWithCypherRetry(t *testing.T) {
	transient := errors.New("no such column: _gql_default_alias_1.id")

	t.Run("succeeds first try", func(t *testing.T) {
		calls := 0
		err := withCypherRetry(func() error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("err=%v calls=%d, want nil/1", err, calls)
		}
	})

	t.Run("non-transient returns immediately", func(t *testing.T) {
		other := errors.New("syntax error")
		calls := 0
		err := withCypherRetry(func() error { calls++; return other })
		if !errors.Is(err, other) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want syntax error/1", err, calls)
		}
	})

	t.Run("retries transient then succeeds", func(t *testing.T) {
		calls := 0
		err := withCypherRetry(func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err=%v calls=%d, want nil/3", err, calls)
		}
	})

	t.Run("gives up after max attempts returning the transient error", func(t *testing.T) {
		calls := 0
		err := withCypherRetry(func() error { calls++; return transient })
		if !isTransientCypherError(err) {
			t.Fatalf("err=%v, want transient", err)
		}
		if calls < 2 {
			t.Fatalf("calls=%d, want multiple attempts", calls)
		}
	})
}
