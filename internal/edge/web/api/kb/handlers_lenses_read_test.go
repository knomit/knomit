package kb

import (
	"math"
	"testing"
)

// An offset near MaxInt overflows offset+limit into a NEGATIVE depth, which
// clears the `> maxLensRecencyDepth` test rather than tripping it and reaches
// the store as `LIMIT -1` — SQLite's spelling of NO limit. The backstop would
// then do the exact opposite of its stated job: one absurd offset asking every
// mount to materialise its whole corpus.
func TestLensFanoutDepth_OverflowingOffsetStillClamps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		offset, limit int
		want          int
	}{
		{"ordinary page", 0, 50, 50},
		{"deep but representable", 9000, 500, 9500},
		{"over the cap", 20000, 500, maxLensRecencyDepth},
		{"overflows to negative", math.MaxInt - 10, 500, maxLensRecencyDepth},
		{"overflows exactly at MaxInt+1", math.MaxInt, 1, maxLensRecencyDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lensFanoutDepth("", tc.offset, tc.limit)
			if got != tc.want {
				t.Fatalf("lensFanoutDepth(\"\", %d, %d): got %d, want %d", tc.offset, tc.limit, got, tc.want)
			}
			if got <= 0 {
				t.Fatalf("depth %d is not a bound: a non-positive Limit means NO limit in the store", got)
			}
		})
	}
}
