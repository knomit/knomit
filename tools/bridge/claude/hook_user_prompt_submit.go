package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"
)

type userPromptSubmitInput struct {
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt"`
}

// designIntentPattern matches phrases that signal the user is starting
// non-trivial design or implementation work — the moment when /knomit-recall
// is most valuable.
//
// Tuned conservatively: false positives are cheap (one extra recall call,
// invariants surfaced); false negatives mean the agent goes in blind. So
// the pattern errs toward firing.
var designIntentPattern = regexp.MustCompile(
	`(?i)\b(implement|redesign|refactor|rework|add (a )?(new )?(feature|support|endpoint|hook|skill|tool)|build (a )?(new )?|design (a |the )?|how should (we|i) (build|approach|implement|design|architect)|let's (build|design|add|implement|refactor)|what'?s the best way to (build|design|implement|add))\b`,
)

// hookUserPromptSubmit fires when the user submits a prompt. If the prompt
// looks like design or implementation intent, nudges CC to run /knomit-recall
// before diving in.
//
// CC's UserPromptSubmit hook protocol: stdout (plain or JSON additionalContext)
// is injected before the agent processes the prompt.
func hookUserPromptSubmit(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "user-prompt-submit").Bool("emitted", emitted)
		if !emitted && skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in userPromptSubmitInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		skipReason = "empty_prompt"
		return nil
	}
	if !designIntentPattern.MatchString(prompt) {
		skipReason = "no_design_intent"
		return nil
	}

	repo := repoFromMCP(in.Cwd)
	msg := fmt.Sprintf(
		"This prompt looks like design or implementation intent. Before brainstorming or touching code, run `/knomit-recall <area>` against the %s knowledge base to surface load-bearing invariants, prior decisions, and anti-patterns. After recall, verify the 3–5 load-bearing claims your work will depend on against HEAD (see the knomit-recall skill for the verification handshake).",
		repo,
	)
	if err := emitAdditionalContext(w, msg); err != nil {
		return err
	}
	emitted = true
	return nil
}
