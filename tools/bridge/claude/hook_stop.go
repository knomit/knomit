package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const stopRateLimit = 5

type stopInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookStop fires at end of every assistant turn. Rate-limited to one nudge
// per stopRateLimit turns to keep noise down.
func hookStop(r io.Reader, w io.Writer) error {
	var in stopInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil
	}

	if !rateLimitFire() {
		return nil
	}

	blocks, err := parseTranscript(in.TranscriptPath, 6)
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
		return nil
	}

	ctx := fmt.Sprintf(
		"This turn produced capture-worthy moments:\n%s\n\nConsider /knomit-remember before moving on.",
		strings.Join(hits, "\n"),
	)
	return emitAdditionalContext(w, ctx)
}

// rateLimitFire returns true at most once per stopRateLimit calls. Uses a
// counter file in the OS temp dir keyed only by the binary (per-machine,
// not per-project — multi-project rate-sharing is a known limitation).
func rateLimitFire() bool {
	tmpDir := os.TempDir()
	counterPath := filepath.Join(tmpDir, "knomit-stop-rate")
	cur := 0
	if data, err := os.ReadFile(counterPath); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			cur = v
		}
	}
	next := cur + 1
	if next >= stopRateLimit {
		_ = os.WriteFile(counterPath, []byte("0"), 0o644)
		return true
	}
	_ = os.WriteFile(counterPath, []byte(strconv.Itoa(next)), 0o644)
	return false
}
