package main

import (
	"errors"
	"fmt"
	"testing"

	"knomit/cmd"
)

// TestExitCodeOf: the path main takes from a command's error to the process
// exit code. A wrapped cmd.ExitCodeError keeps its code (verify ci's 1 and 2
// reach the forge job); any other error exits 1.
func TestExitCodeOf(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&cmd.ExitCodeError{Code: 2, Err: errors.New("could not run")}, 2},
		{fmt.Errorf("wrapped: %w", &cmd.ExitCodeError{Code: 2, Err: errors.New("x")}), 2},
		{&cmd.ExitCodeError{Code: 1, Err: errors.New("blocked")}, 1},
		{errors.New("plain"), 1},
	} {
		if got := exitCodeOf(tc.err); got != tc.want {
			t.Errorf("exitCodeOf(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
