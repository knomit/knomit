package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Off a terminal the UI must emit plain, one-line-per-stage output: no ANSI
// colour, no carriage returns. Otherwise a CI log or a `> out.txt` fills with
// escape sequences and half-overwritten lines.
func TestUI_NonTTYOutputIsPlainAndLineBased(t *testing.T) {
	var buf bytes.Buffer
	u := newUI(&buf)
	u.Banner("0.5.0.abc1234")
	u.Step("Fetching", "https://example.com/kb")
	u.Update("commits 1200") // must be suppressed off a terminal
	u.Update("commits 2400")
	u.Done("3 branches")
	u.Step("Reading", "main")
	u.Done("297 facts")

	got := buf.String()
	require.NotContains(t, got, "\x1b[", "no ANSI escapes off a terminal")
	require.NotContains(t, got, "\r", "no carriage returns off a terminal")
	require.NotContains(t, got, "commits 1200", "in-place updates must not spam a pipe")
	require.NotContains(t, got, "commits 2400")

	// Exactly one line per completed stage, each naming its label and summary.
	var stages []string
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "✓") {
			stages = append(stages, l)
		}
	}
	require.Len(t, stages, 2, "one line per stage, got:\n%s", got)
	require.Contains(t, stages[0], "Fetching")
	require.Contains(t, stages[0], "3 branches")
	require.Contains(t, stages[1], "Reading")
	require.Contains(t, stages[1], "297 facts")
}

// A stage announces itself BEFORE the work runs. That is the whole point of
// the progress output: a silent process is indistinguishable from a hung one,
// which is what made a slow clone look stuck.
func TestUI_TTYAnnouncesStageBeforeWork(t *testing.T) {
	var buf bytes.Buffer
	u := newUI(&buf)
	u.tty = true // simulate a terminal without needing a real one
	u.Step("Fetching", "https://example.com/kb")

	require.Contains(t, buf.String(), "Fetching",
		"the stage must be visible before it completes")
	require.Contains(t, buf.String(), "https://example.com/kb",
		"and must say what it is working on")
	require.NotContains(t, buf.String(), "✓", "not marked done until it is done")
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{450 * time.Millisecond, "450ms"},
		{2500 * time.Millisecond, "2.5s"},
		{90 * time.Second, "1m30s"},
		{3725 * time.Second, "62m05s"},
	} {
		require.Equal(t, tc.want, humanDuration(tc.d))
	}
}

// visibleLen underpins the in-place redraw: it decides how much of the
// previous line to erase. Counting escape bytes or UTF-8 bytes instead of
// printable runes leaves debris on screen.
func TestVisibleLen_IgnoresEscapesAndCountsRunes(t *testing.T) {
	require.Equal(t, 5, visibleLen("hello"))
	require.Equal(t, 5, visibleLen("\x1b[1mhello\x1b[0m"))
	require.Equal(t, 3, visibleLen("✓ ·"), "multi-byte runes count once each")
}

// throttledCount must pass the FIRST tick through — a throttle that swallows
// it would leave the stage showing a stale count until the second tick, which
// on a fast stage never arrives.
func TestThrottledCount_EmitsFirstTick(t *testing.T) {
	var seen []int
	fn := throttledCount(func(done int) { seen = append(seen, done) })
	fn(1)
	fn(2) // within the interval — dropped
	require.Equal(t, []int{1}, seen)
}
