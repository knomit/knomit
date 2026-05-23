package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog/log"
)

type preCompactInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookPreCompact scans the most recent transcript window for
// capture-worthy moments via knomit /detect and nudges if any score above
// threshold (0.7).
func hookPreCompact(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		blocksLen  int
		hitsCount  int
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "pre-compact").Bool("emitted", emitted).Int("blocks", blocksLen)
		if emitted {
			ev.Int("hits", hitsCount).Msg("hook result")
			return
		}
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in preCompactInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}
	blocks, err := parseTranscript(in.TranscriptPath, 24)
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
		maxScore := 0.0
		var matched []string
		for _, s := range b.Signals {
			if s.Score > maxScore {
				maxScore = s.Score
			}
			if s.Score > 0.7 {
				matched = append(matched, s.Intent)
			}
		}
		if maxScore > 0.7 {
			hits = append(hits, fmt.Sprintf("  - %s: block %d", strings.Join(matched, ","), b.Index))
		}
	}
	if len(hits) == 0 {
		skipReason = "no_hits_above_threshold"
		return nil
	}

	ctx := fmt.Sprintf(
		"Before compaction, these recent moments look capture-worthy:\n%s\n\nRun /knomit-remember or /knomit-decided if you want any of them preserved.",
		strings.Join(hits, "\n"),
	)
	if err := emitAdditionalContext(w, ctx); err != nil {
		return err
	}
	emitted = true
	hitsCount = len(hits)
	return nil
}
