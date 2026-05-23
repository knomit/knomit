package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

const stopRateLimit = 5

type stopInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookStop fires at end of every assistant turn. Rate-limited to one nudge
// per stopRateLimit turns to keep noise down.
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

	if !rateLimitFire() {
		skipReason = "rate_limited"
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
