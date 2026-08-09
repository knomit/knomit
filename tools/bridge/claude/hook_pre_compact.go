package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rs/zerolog/log"
)

const (
	preCompactMaxScan  = 50
	preCompactMaxEmits = 8
)

type preCompactInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookPreCompact scans a wider window than hookStop just before the
// transcript is compacted away, surfacing capture candidates so they
// aren't lost to compaction. Not rate-limited — pre-compaction is rare.
func hookPreCompact(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		hitsCount  int
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "pre-compact").Bool("emitted", emitted)
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

	var sb strings.Builder
	hits := 0
	prevRole := ""
	seen := make(map[string]bool)

	err := scanTranscript(in.TranscriptPath, preCompactMaxScan, func(role, text string) bool {
		for _, m := range matchIntents(role, text, prevRole) {
			key := m.intent + "|" + m.quote
			if seen[key] {
				continue
			}
			seen[key] = true
			if hits == 0 {
				sb.WriteString("Before compaction, these moments look capture-worthy:\n")
			}
			fmt.Fprintf(&sb, "\n- %s (%s): %q\n", m.intent, role, m.quote)
			hits++
			if hits >= preCompactMaxEmits {
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
	sb.WriteString("\nRun /knomit-remember or /knomit-decided to preserve them.\n")

	if err := emitAdditionalContext(w, "PreCompact", sb.String()); err != nil {
		return err
	}
	emitted = true
	hitsCount = hits
	return nil
}
