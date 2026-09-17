package claude

import (
	"encoding/json"
	"io"
	"os"

	"github.com/rs/zerolog/log"

	"knomit/tools/bridge/memguard"
)

type memoryGuardInput struct {
	// HookEventName is the event CC dispatched this hook for. It is echoed
	// back in hookSpecificOutput — see wiredEvent.
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
		Command  string `json:"command"`
	} `json:"tool_input"`
}

// permissionDecision is the PreToolUse deny envelope.
type permissionDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// hookMemoryGuard fires before Write, Edit, MultiEdit and Bash, and denies the
// call when it would put a team-relevant note into Claude Code's private
// auto-memory directory instead of knomit.
//
// It FAILS OPEN everywhere it is unsure: a malformed payload, an unrecognised
// tool, an unreadable file, a path outside the directory — all exit silently,
// allowing the call. A guard on Write|Edit|MultiEdit|Bash sees nearly every
// action an agent takes, so a false deny would block real work and get the
// hook removed; then it protects nothing. Only a positively recognised
// violation is denied.
func hookMemoryGuard(r io.Reader, w io.Writer) error {
	var (
		denied   bool
		toolName string
	)
	defer func() {
		ev := log.Info().Str("event", "memory-guard").Bool("denied", denied)
		if toolName != "" {
			ev.Str("tool", toolName)
		}
		ev.Msg("hook result")
	}()

	var in memoryGuardInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil // malformed payload: allow, never deny unrelated work
	}
	toolName = in.ToolName

	var d memguard.Decision
	switch in.ToolName {
	case "Write", "Edit", "MultiEdit":
		// Write carries the new content; an Edit does not, so the frontmatter
		// has to come off the file already on disk. An unreadable file yields
		// "", and the path alone then decides.
		content := in.ToolInput.Content
		if content == "" {
			if b, err := os.ReadFile(in.ToolInput.FilePath); err == nil {
				content = string(b)
			}
		}
		d = memguard.CheckFileWrite(in.ToolInput.FilePath, content)
	case "Bash":
		d = memguard.CheckBash(in.ToolInput.Command)
	default:
		return nil
	}
	if !d.Deny {
		return nil
	}

	var out permissionDecision
	out.HookSpecificOutput.HookEventName = wiredEvent(in.HookEventName, "PreToolUse")
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = d.Reason
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return err
	}
	denied = true
	return nil
}
