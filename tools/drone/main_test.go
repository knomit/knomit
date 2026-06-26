package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStreamEvents_LargeLine is the regression test for the stream-drain
// deadlock: a single event far larger than any fixed scanner buffer must be
// copied through in full, and the reader must keep going past it. The old
// bufio.Scanner (16 MB token ceiling) aborted on such a line, left claude's
// stdout undrained, and deadlocked cmd.Wait().
func TestStreamEvents_LargeLine(t *testing.T) {
	huge := strings.Repeat("x", 20*1024*1024) // 20 MB, well over the old 16 MB cap
	input := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"assistant","big":"` + huge + `"}` + "\n" +
		`{"type":"result","result":"done"}` + "\n"

	var out bytes.Buffer
	streamEvents(strings.NewReader(input), &out)

	// Every byte is drained through to the log, including the oversized line
	// and — crucially — the events that follow it.
	assert.Equal(t, input, out.String())
}

// TestStreamEvents_NoTrailingNewline ensures a final partial line (e.g. a
// crash mid-write) is still flushed rather than dropped at EOF.
func TestStreamEvents_NoTrailingNewline(t *testing.T) {
	input := `{"type":"result","result":"ok"}` + "\n" + `{"type":"assistant"}` // no final \n

	var out bytes.Buffer
	streamEvents(strings.NewReader(input), &out)

	assert.Equal(t, input, out.String())
}

func TestSplitList(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"flag style stays intact", []string{"a.com", "b.com"}, []string{"a.com", "b.com"}},
		{"comma-joined env value splits", []string{"a.com,b.com"}, []string{"a.com", "b.com"}},
		{"whitespace already split by viper", []string{"a.com", "b.com"}, []string{"a.com", "b.com"}},
		{"mixed commas and entries", []string{"a.com,b.com", "c.com"}, []string{"a.com", "b.com", "c.com"}},
		{"trims and drops blanks", []string{" a.com , ,b.com", "  ", ""}, []string{"a.com", "b.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitList(tt.in))
		})
	}
}

func TestWorktreePath(t *testing.T) {
	got := worktreePath("/repo", "auto/my-plan-20060102-1504")
	want := filepath.Join("/repo", ".claude", "worktrees", "auto-my-plan-20060102-1504")
	assert.Equal(t, want, got)

	// Two runs with distinct branch names land in distinct directories, which
	// is what lets parallel drones avoid colliding on the working tree.
	a := worktreePath("/repo", "auto/x-1")
	b := worktreePath("/repo", "auto/x-2")
	assert.NotEqual(t, a, b)
}

func TestSanitizeBranch(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"My Plan", "my-plan"},
		{"feat/Foo_Bar", "feat/foo_bar"},
		{"--leading-trailing--", "leading-trailing"},
		{"weird@#chars!", "weird--chars"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, sanitizeBranch(tt.in), tt.in)
	}
}

func TestSiblingPath(t *testing.T) {
	require.Equal(t, "/x/drone-1.prompt.txt", siblingPath("/x/drone-1.jsonl", ".prompt.txt"))
	require.Equal(t, "/x/drone-1.stderr.log", siblingPath("/x/drone-1.jsonl", ".stderr.log"))
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", truncate("abc", 5))
	assert.Equal(t, "abc", truncate("  abc  ", 5)) // trims first, then measures
	assert.Equal(t, "ab…", truncate("abcdef", 2))
}
