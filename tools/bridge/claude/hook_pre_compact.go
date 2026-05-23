package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type preCompactInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookPreCompact scans the most recent transcript window for
// capture-worthy moments via knomit /detect and nudges if any score above
// threshold (0.7).
func hookPreCompact(r io.Reader, w io.Writer) error {
	var in preCompactInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil
	}
	blocks, err := parseTranscript(in.TranscriptPath, 24)
	if err != nil || len(blocks) == 0 {
		return nil
	}

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
		return nil
	}

	ctx := fmt.Sprintf(
		"Before compaction, these recent moments look capture-worthy:\n%s\n\nRun /knomit-remember or /knomit-decided if you want any of them preserved.",
		strings.Join(hits, "\n"),
	)
	return emitAdditionalContext(w, ctx)
}
