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

const (
	stopRateLimit = 5
	stopMaxScan   = 20
	stopMaxEmits  = 5
)

type stopInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookStop fires at end of every assistant turn. Rate-limited to one nudge
// per stopRateLimit turns. On fire, scans the last stopMaxScan dialogue
// messages of the transcript (tail-read), regex-matches each for intent
// patterns, and emits up to stopMaxEmits quoted candidates via
// additionalContext.
func hookStop(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		hitsCount  int
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "stop").Bool("emitted", emitted)
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

	var sb strings.Builder
	hits := 0
	prevRole := ""

	err := scanTranscript(in.TranscriptPath, stopMaxScan, func(role, text string) bool {
		for _, m := range matchIntents(role, text, prevRole) {
			if hits == 0 {
				sb.WriteString("Capture candidates from this turn:\n")
			}
			fmt.Fprintf(&sb, "\n- %s (%s): %q\n", m.intent, role, m.quote)
			hits++
			if hits >= stopMaxEmits {
				return false
			}
		}
		prevRole = role
		return true
	})
	if err != nil {
		skipReason = "transcript_unreadable"
		return nil
	}
	if hits == 0 {
		skipReason = "no_hits"
		return nil
	}
	sb.WriteString("\nIf these aren't already in knomit, /knomit-remember.\n")

	if err := emitAdditionalContext(w, sb.String()); err != nil {
		return err
	}
	emitted = true
	hitsCount = hits
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
