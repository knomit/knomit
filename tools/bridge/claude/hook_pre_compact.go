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
//
// PreCompact has no hookSpecificOutput variant in CC's output schema and no
// additionalContext channel: a JSON envelope is rejected outright ("Hook JSON
// output validation failed"), which discards the whole nudge. CC instead takes
// this hook's plain stdout as the compaction's custom instructions, so we write
// bare text like session-start does.
//
// Crucially, those custom instructions are spliced into the *summarizer's*
// prompt as "Additional Instructions:", under a preamble that reads "CRITICAL:
// Respond with TEXT ONLY. Do NOT call any tools." The reader of this text is
// therefore the summarizer, not the working agent, and it cannot run a slash
// command however firmly we ask. The copy below is written as summarization
// guidance — carry the candidates into the summary verbatim — so the working
// agent finds them on the other side of compaction and captures them then.
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
				sb.WriteString("These moments are knomit capture candidates. Reproduce them verbatim in the summary, under a \"knomit capture candidates\" heading, so they survive compaction:\n")
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
	sb.WriteString("\nDo not summarize them away or merge them into other points — carry the quotes through intact, and note that they are still to be captured with /knomit-remember or /knomit-decided.\n")

	if _, err := w.Write([]byte(sb.String())); err != nil {
		return err
	}
	emitted = true
	hitsCount = hits
	return nil
}
