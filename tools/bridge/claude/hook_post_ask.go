package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog/log"
)

type postAskInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
		} `json:"questions"`
	} `json:"tool_input"`
	ToolResponse struct {
		Answers map[string]string `json:"answers"`
	} `json:"tool_response"`
}

// hookPostAsk fires after every AskUserQuestion call. Almost every resolved
// AskUserQuestion is a documented user choice; many are tradeoff resolutions
// that warrant /knomit-decided. We fire unconditionally — the /knomit-decided
// skill itself has the "is this worth recording" filter (preference vs.
// tradeoff). Goal here: surface the moment so it isn't buried by the work it
// just authorized.
func hookPostAsk(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		toolName   string
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "post-ask").Bool("emitted", emitted)
		if toolName != "" {
			ev.Str("tool", toolName)
		}
		if !emitted && skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in postAskInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}
	toolName = in.ToolName
	if in.ToolName != "AskUserQuestion" {
		skipReason = "non_ask_tool"
		return nil
	}

	var sb strings.Builder
	sb.WriteString("You just resolved an AskUserQuestion. If it picked between options with non-obvious tradeoffs (not just a preference or a path-disambiguator), capture it now with /knomit-decided BEFORE starting the work it authorized.\n")
	for _, q := range in.ToolInput.Questions {
		if q.Question == "" {
			continue
		}
		ans, ok := in.ToolResponse.Answers[q.Question]
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "\n  Q: %s\n  A: %s\n", q.Question, ans)
	}
	sb.WriteString("\nSkip /knomit-decided only if this was a clarifying preference (theme color, file path, etc.) — not a tradeoff.\n")

	if err := emitAdditionalContext(w, sb.String()); err != nil {
		return err
	}
	emitted = true
	return nil
}
