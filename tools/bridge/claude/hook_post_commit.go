package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type postToolUseInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	ToolOutput struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	} `json:"tool_output"`
}

var commitMarkers = regexp.MustCompile(`(?i)\b(fix|refactor|decided|invariant|gotcha):`)

// hookPostCommit fires after every Bash tool use. Filters to git commit
// calls and nudges CC to /knomit-remember substantive ones.
func hookPostCommit(r io.Reader, w io.Writer) error {
	var in postToolUseInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(in.ToolInput.Command), "git commit") {
		return nil
	}
	stdout := in.ToolOutput.Stdout
	if len(stdout) < 60 && !commitMarkers.MatchString(stdout) {
		return nil
	}
	msg := fmt.Sprintf(
		"This commit looks substantive. Run /knomit-remember to capture as a fact, or /knomit-decided if the commit codifies a design choice.\n\nCommit subject:\n%s",
		stdout,
	)
	return emitAdditionalContext(w, msg)
}
