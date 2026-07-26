package llm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeCLIAdapter_UsesSystemPromptFlag regresses a real bug found while
// exercising this adapter against the actual installed `claude` CLI
// (v2.1.220): the adapter passed --system, but the real CLI only accepts
// --system-prompt and rejects --system with "error: unknown option
// '--system'" — every claudecli-backed call failed outright. No prior test
// exercised the real subprocess invocation, so this went undetected.
//
// A fake `claude` script stands in for the real CLI: it rejects --system
// exactly like the real one does, and echoes back the --system-prompt value
// plus stdin so the test can assert both are wired correctly.
func TestClaudeCLIAdapter_UsesSystemPromptFlag(t *testing.T) {
	const fakeClaudeScript = `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "--system" ]; then
    echo "error: unknown option '--system'" >&2
    exit 1
  fi
done
print_next=0
for arg in "$@"; do
  if [ "$print_next" = "1" ]; then
    echo "SYSTEM:$arg"
    print_next=0
  fi
  if [ "$arg" = "--system-prompt" ]; then
    print_next=1
  fi
done
echo "STDIN:$(cat)"
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(scriptPath, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Sanity: LookPath must resolve to the fake script, not a real installed
	// claude, or this test would silently ride on whatever happens to be on
	// the host machine's PATH instead of testing anything.
	if resolved, err := exec.LookPath("claude"); err != nil || resolved != scriptPath {
		t.Fatalf("exec.LookPath(claude) = %q, %v; want %q", resolved, err, scriptPath)
	}

	a := NewClaudeCLIAdapter("")
	out, err := a.Complete(context.Background(), "you are a test", []Message{{Role: "user", Content: "hello"}}, CompletionOptions{}, nil)
	if err != nil {
		t.Fatalf("Complete returned error (adapter likely passing --system again): %v", err)
	}
	if !strings.Contains(out, "SYSTEM:you are a test") {
		t.Errorf("system prompt not passed via --system-prompt; got: %q", out)
	}
	if !strings.Contains(out, "STDIN:hello") {
		t.Errorf("user content not passed via stdin; got: %q", out)
	}
}

// TestClaudeCLIAdapter_RunsOutsideCallerCwd regresses a real bug found while
// generating synthetic test corpora with this adapter from within this very
// repo: `claude -p` auto-discovers CLAUDE.md and other project context from
// its working directory, so every completion silently came back describing
// knomit itself instead of the requested independent content. The adapter
// must run the subprocess from a neutral directory, not whatever the caller's
// process cwd happens to be.
func TestClaudeCLIAdapter_RunsOutsideCallerCwd(t *testing.T) {
	const fakeClaudeScript = `#!/bin/sh
echo "PWD:$(pwd -P)"
cat >/dev/null
`
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "claude")
	if err := os.WriteFile(scriptPath, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A project-shaped directory the caller's process happens to be in —
	// stands in for a real repo with a CLAUDE.md the subprocess must NOT see.
	callerCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(callerCwd, "CLAUDE.md"), []byte("project context"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(callerCwd)

	a := NewClaudeCLIAdapter("")
	out, err := a.Complete(context.Background(), "sys", nil, CompletionOptions{}, nil)
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if strings.Contains(out, "PWD:"+callerCwd) {
		t.Errorf("subprocess ran in the caller's cwd (%s), which would leak its CLAUDE.md/project context; got: %q", callerCwd, out)
	}
}

// TestClaudeCLIAdapter_AllowedTools verifies SetAllowedTools: unset by
// default (no --allowedTools flag at all, so existing callers' behavior is
// unchanged), and passed through verbatim when a caller opts in.
func TestClaudeCLIAdapter_AllowedTools(t *testing.T) {
	const fakeClaudeScript = `#!/bin/sh
echo "ARGS:$@"
cat >/dev/null
`
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "claude")
	if err := os.WriteFile(scriptPath, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", scriptDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("unset by default", func(t *testing.T) {
		a := NewClaudeCLIAdapter("")
		out, err := a.Complete(context.Background(), "sys", nil, CompletionOptions{}, nil)
		if err != nil {
			t.Fatalf("Complete returned error: %v", err)
		}
		if strings.Contains(out, "--allowedTools") {
			t.Errorf("--allowedTools present without SetAllowedTools being called; got: %q", out)
		}
	})

	t.Run("passed through when set", func(t *testing.T) {
		a := NewClaudeCLIAdapter("")
		a.SetAllowedTools([]string{"WebSearch"})
		out, err := a.Complete(context.Background(), "sys", nil, CompletionOptions{}, nil)
		if err != nil {
			t.Fatalf("Complete returned error: %v", err)
		}
		if !strings.Contains(out, "--allowedTools WebSearch") {
			t.Errorf("expected --allowedTools WebSearch in args; got: %q", out)
		}
	})
}
