package claude

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/rs/zerolog/log"

	"knomit/tools/bridge/knomitapi"
)

type sessionStartInput struct {
	Cwd            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// hookSessionStart fires once per CC session. CC auto-wraps plain stdout from
// this hook as a system reminder, so we emit plain text (no JSON envelope).
func hookSessionStart(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		stats      knomitapi.Stats
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "session-start").Bool("emitted", emitted)
		// skip_reason is logged whenever it is set, INDEPENDENT of emitted.
		// The two are not mutually exclusive: the multiple-servers skip still
		// writes a notice to the user, and logging only on the not-emitted
		// branch made that line indistinguishable from a healthy emission.
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		if emitted {
			ev.Int("globals", stats.Globals).
				Int("invariants_fallback", stats.InvariantsFallback).
				Int("recent", stats.Recent)
		}
		ev.Msg("hook result")
	}()

	var in sessionStartInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil // exit cleanly on bad input — don't disrupt session start
	}
	repo, skip := resolveWriteRepo(in.Cwd)
	if skip != "" {
		skipReason = skip
		// Every other skip reason is transient or benign. This one is a
		// misconfiguration that will never resolve on its own, and the user
		// cannot see the log field — so say it out loud.
		if skip == skipMultipleKnomitServers {
			_, err := fmt.Fprint(w, "knomit hooks are DISABLED: .mcp.json configures knomit "+
				"servers for more than one scope, so there is no single repo to bind to. "+
				"Leave one scope (one repo or one lens) in this project's .mcp.json to "+
				"re-enable them; duplicate entries naming the SAME scope are fine.\n")
			if err != nil {
				return err
			}
			emitted = true
		}
		return nil
	}
	branch := knomitapi.AgentBranch(repo)
	if branch == "" {
		skipReason = "no_agent_branch"
		return nil
	}

	text, st := knomitapi.SessionContext(repo, branch)
	stats = st
	if text == "" {
		skipReason = st.SkipReason
		return nil
	}
	if _, err := w.Write([]byte(text)); err != nil {
		return err
	}
	emitted = true
	return nil
}
