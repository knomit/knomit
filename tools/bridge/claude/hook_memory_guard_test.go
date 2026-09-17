package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMemoryGuard(t *testing.T, payload string) (denied bool, reason string) {
	t.Helper()
	var out bytes.Buffer
	if err := hookMemoryGuard(strings.NewReader(payload), &out); err != nil {
		t.Fatalf("hookMemoryGuard: %v", err)
	}
	if out.Len() == 0 {
		return false, ""
	}
	var got permissionDecision
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.String(), err)
	}
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", got.HookSpecificOutput.HookEventName)
	}
	return got.HookSpecificOutput.PermissionDecision == "deny", got.HookSpecificOutput.PermissionDecisionReason
}

func TestHookMemoryGuard_WriteAndBash(t *testing.T) {
	dir := "/home/me/.claude/projects/-home-me-proj/memory"
	proj := "---\nname: n\nmetadata:\n  type: project\n---\nbody\n"
	user := "---\nname: n\nmetadata:\n  type: user\n---\nbody\n"

	for _, c := range []struct {
		name, payload string
		deny          bool
	}{
		{"project write denied", writePayload(dir+"/lesson.md", proj), true},
		{"user write allowed", writePayload(dir+"/who.md", user), false},
		{"MEMORY.md write allowed", writePayload(dir+"/MEMORY.md", "# index"), false},
		{"repo file allowed", writePayload("/home/me/proj/x.go", proj), false},
		{"bash append denied", bashPayload("echo x >> " + dir + "/MEMORY.md"), true},
		{"bash rm denied", bashPayload("rm " + dir + "/x.md"), true},
		{"bash read allowed", bashPayload("cat " + dir + "/MEMORY.md"), false},
		{"bash echo of read allowed", bashPayload("echo $(sed -n 1p " + dir + "/MEMORY.md)"), false},
		{"unrelated tool allowed", `{"hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"x"}}`, false},
		// A malformed payload must never deny: this hook sees nearly every
		// action, so failing closed here would block unrelated work.
		{"malformed json allowed", `{"tool_name":`, false},
		{"empty payload allowed", ``, false},
	} {
		denied, reason := runMemoryGuard(t, c.payload)
		if denied != c.deny {
			t.Errorf("%s: denied=%v want %v", c.name, denied, c.deny)
		}
		if denied && !strings.Contains(reason, "/knomit-remember") {
			t.Errorf("%s: reason must route the agent to knomit, got %q", c.name, reason)
		}
	}
}

// An Edit carries no content, so the guard must read the file already on disk
// to tell a user memory from a team one.
func TestHookMemoryGuard_EditReadsFileOnDisk(t *testing.T) {
	root := t.TempDir()
	memDir := filepath.Join(root, ".claude", "projects", "-proj", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projFile := filepath.Join(memDir, "lesson.md")
	userFile := filepath.Join(memDir, "who.md")
	if err := os.WriteFile(projFile, []byte("---\nmetadata:\n  type: project\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFile, []byte("---\nmetadata:\n  type: user\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if denied, _ := runMemoryGuard(t, editPayload(projFile)); !denied {
		t.Error("editing an existing project-type memory must be denied")
	}
	if denied, _ := runMemoryGuard(t, editPayload(userFile)); denied {
		t.Error("editing an existing user-type memory must be allowed")
	}
}

func writePayload(path, content string) string {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": path, "content": content},
	})
	return string(b)
}

func editPayload(path string) string {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": path, "old_string": "a", "new_string": "b"},
	})
	return string(b)
}

func bashPayload(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd},
	})
	return string(b)
}
