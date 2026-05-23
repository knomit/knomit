package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
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
	var (
		emitted    bool
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "post-commit").Bool("emitted", emitted)
		if !emitted && skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in postToolUseInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(in.ToolInput.Command), "git commit") {
		skipReason = "not_git_commit"
		return nil
	}
	stdout := in.ToolOutput.Stdout
	if len(stdout) < 60 && !commitMarkers.MatchString(stdout) {
		skipReason = "nonsubstantive"
		return nil
	}
	msg := fmt.Sprintf(
		"This commit looks substantive. Run /knomit-remember to capture as a fact, or /knomit-decided if the commit codifies a design choice.\n\nCommit subject:\n%s",
		stdout,
	)
	if err := emitAdditionalContext(w, msg); err != nil {
		return err
	}
	emitted = true
	return nil
}
