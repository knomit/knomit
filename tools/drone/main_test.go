package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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
	streamEvents(strings.NewReader(input), &out, nil)

	// Every byte is drained through to the log, including the oversized line
	// and — crucially — the events that follow it.
	assert.Equal(t, input, out.String())
}

// TestStreamEvents_NoTrailingNewline ensures a final partial line (e.g. a
// crash mid-write) is still flushed rather than dropped at EOF.
func TestStreamEvents_NoTrailingNewline(t *testing.T) {
	input := `{"type":"result","result":"ok"}` + "\n" + `{"type":"assistant"}` // no final \n

	var out bytes.Buffer
	streamEvents(strings.NewReader(input), &out, nil)

	assert.Equal(t, input, out.String())
}

// TestStreamEvents_Scrubs is the regression test for token leakage into the
// audit log: a secret present in the streamed events must be redacted before
// it is teed to disk.
func TestStreamEvents_Scrubs(t *testing.T) {
	input := `{"type":"result","result":"pushed with gho_supersecrettoken123"}` + "\n"

	var out bytes.Buffer
	streamEvents(strings.NewReader(input), &out, []string{"gho_supersecrettoken123"})

	assert.NotContains(t, out.String(), "gho_supersecrettoken123")
	assert.Contains(t, out.String(), "***REDACTED***")
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

func TestInstanceName(t *testing.T) {
	// auto branch -> per-run folder is just plan + ksuid (no "auto/").
	assert.Equal(t, "my-plan-2abcKSUID", instanceName("auto/my-plan-2abcKSUID"))
	// a custom branch keeps its name, slashes flattened so it's one folder.
	assert.Equal(t, "feat-foo", instanceName("feat/foo"))
	assert.Equal(t, "plain", instanceName("plain"))
}

func TestSiblingPath(t *testing.T) {
	require.Equal(t, "/x/drone-1.prompt.txt", siblingPath("/x/drone-1.jsonl", ".prompt.txt"))
	require.Equal(t, "/x/drone-1.stderr.log", siblingPath("/x/drone-1.jsonl", ".stderr.log"))
}

// TestBuildSettings_SchemaShape is the regression test for the misplaced
// network keys: claude's --settings schema silently drops unknown keys, so
// allowedDomains/allowLocalBinding MUST sit under sandbox.network (not directly
// under sandbox) and allowWrite under sandbox.filesystem, or the guard is
// quietly disabled. Verified empirically against claude 2.1.150.
func TestBuildSettings_SchemaShape(t *testing.T) {
	cfg := &config{sandbox: true, allowLocal: true, repo: "/repo", worktree: "/repo/.claude/worktrees/wt"}
	out, err := buildSettings(cfg)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	sb := m["sandbox"].(map[string]any)

	// network keys live under sandbox.network, never flat under sandbox.
	assert.NotContains(t, sb, "allowedDomains", "allowedDomains must be nested under network")
	network := sb["network"].(map[string]any)
	assert.Contains(t, network, "allowedDomains")
	assert.Equal(t, true, network["allowLocalBinding"])

	// allowWrite lives under sandbox.filesystem.
	fs := sb["filesystem"].(map[string]any)
	assert.Contains(t, fs, "allowWrite")

	// allowLocal off omits the binding entirely (no localhost reach).
	off, err := buildSettings(&config{sandbox: true, allowLocal: false, repo: "/repo", worktree: "/repo/wt"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(off), &m))
	netOff := m["sandbox"].(map[string]any)["network"].(map[string]any)
	assert.NotContains(t, netOff, "allowLocalBinding")
}

// TestLinkArtifacts symlinks a gitignored build dir from the repo into the
// worktree (so e.g. ${CLAUDE_PROJECT_DIR}/dist/<bin> resolves), and skips a
// configured path that doesn't exist in the repo rather than failing.
func TestLinkArtifacts(t *testing.T) {
	repo := t.TempDir()
	worktree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "dist", "bin"), []byte("x"), 0o755))

	cfg := &config{repo: repo, worktree: worktree, links: []string{"dist", "missing"}}
	require.NoError(t, linkArtifacts(cfg))

	// dist is reachable through the worktree via the symlink...
	got, err := os.ReadFile(filepath.Join(worktree, "dist", "bin"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	// ...and the missing path was skipped, not created.
	_, err = os.Lstat(filepath.Join(worktree, "missing"))
	assert.True(t, os.IsNotExist(err))
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", truncate("abc", 5))
	assert.Equal(t, "abc", truncate("  abc  ", 5)) // trims first, then measures
	assert.Equal(t, "ab…", truncate("abcdef", 2))

	// Regression: a cut landing inside a multi-byte rune must back up to a rune
	// boundary, never emit a partial rune. "a—b" is a(1) + em-dash(3) + b(1);
	// cutting at byte 2 lands mid-dash, so the result must drop the whole dash.
	got := truncate("a—b", 2)
	assert.True(t, utf8.ValidString(got), "truncate must not emit invalid UTF-8: %q", got)
	assert.Equal(t, "a…", got)
}
