package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog/log"
)

type stopInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookStop fires at end of every assistant turn. Scores the recent transcript
// for capture-worthy moments and nudges /knomit-remember when any intent
// scores above threshold. The scoring threshold (0.75) is the rate control —
// most turns score below it and emit nothing. There is no inter-turn rate
// limit: bridge invocations are one-shot subprocesses, so any cross-invocation
// counter would need persistent state, and persistent state in /tmp or
// similar is a bug magnet (cross-user, cross-project, world-writable, dies
// on tmpfs reboot). If output noise becomes a problem, raise the threshold.
func hookStop(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		blocksLen  int
		hitsCount  int
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "stop").Bool("emitted", emitted)
		if blocksLen > 0 {
			ev.Int("blocks", blocksLen)
		}
		if emitted {
			ev.Int("hits", hitsCount).Msg("hook result")
			return
		}
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in stopInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}

	blocks, err := parseTranscript(in.TranscriptPath, 6)
	if err != nil || len(blocks) == 0 {
		skipReason = "no_transcript_blocks"
		return nil
	}
	blocksLen = len(blocks)

	intents := []string{"correction", "discovery", "decision", "fix-bug", "gotcha"}
	var novelty *detectNoveltyContext
	if in.Cwd != "" {
		repo := repoFromMCP(in.Cwd)
		if br := agentBranch(repo); br != "" {
			novelty = &detectNoveltyContext{Repo: repo, Branch: br}
		}
	}

	resp, err := postDetect(blocks, intents, novelty)
	if err != nil {
		skipReason = "detect_failed"
		return nil
	}

	var hits []string
	for _, b := range resp.Blocks {
		var matched []string
		for _, s := range b.Signals {
			if s.Score > 0.75 {
				matched = append(matched, s.Intent)
			}
		}
		if len(matched) > 0 {
			hits = append(hits, fmt.Sprintf("  - %s", strings.Join(matched, ",")))
		}
	}
	if len(hits) == 0 {
		skipReason = "no_hits_above_threshold"
		return nil
	}

	ctx := fmt.Sprintf(
		"This turn produced capture-worthy moments:\n%s\n\nConsider /knomit-remember before moving on.",
		strings.Join(hits, "\n"),
	)
	if err := emitAdditionalContext(w, ctx); err != nil {
		return err
	}
	emitted = true
	hitsCount = len(hits)
	return nil
}
