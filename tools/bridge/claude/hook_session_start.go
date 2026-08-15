package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"
)

type sessionStartInput struct {
	Cwd            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// hookSessionStart fires once per CC session. CC auto-wraps plain stdout
// from this hook as a system reminder, so we emit plain text (no JSON envelope).
func hookSessionStart(r io.Reader, w io.Writer) error {
	var (
		emitted         bool
		globalsCount    int
		invariantsCount int
		recentCount     int
		skipReason      string
	)
	defer func() {
		ev := log.Info().Str("event", "session-start").Bool("emitted", emitted)
		if emitted {
			ev.Int("globals", globalsCount).
				Int("invariants_fallback", invariantsCount).
				Int("recent", recentCount).
				Msg("hook result")
			return
		}
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
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
		return nil
	}
	branch := agentBranch(repo)
	if branch == "" {
		skipReason = "no_agent_branch"
		return nil
	}

	// Fetch a recent window in one round-trip and split it client-side into
	// the invariants list (prefix match on kb/invariants/) and a recent-work
	// list. 200 is plenty for typical KBs and small enough to keep the hook
	// fast. Two scoped /facts?topic= calls would also work; the single window
	// keeps surface area small.
	recentWindow := fetchFacts(sessionStartFactsURL(repo, branch))
	globals := filterGlobalPrinciples(recentWindow, 7)
	recent := topN(recentWindow, 5)

	// Rollout fallback: until designers seed global principles, surface
	// the legacy top-5 invariants block so agents don't regress to zero
	// load-bearing context. Once any global principle exists, this branch
	// goes dark — the principles block takes over.
	var invariantsFallback []factSummary
	if len(globals) == 0 {
		invariantsFallback = filterByPathPrefix(recentWindow, "kb/invariants/", 5)
	}

	if len(globals) == 0 && len(invariantsFallback) == 0 && len(recent) == 0 {
		skipReason = "no_facts"
		return nil
	}

	var sb strings.Builder
	sb.WriteString("Known facts from knomit for this codebase:\n\n")
	if len(globals) > 0 {
		sb.WriteString("PROJECT PRINCIPLES:\n")
		for _, f := range globals {
			short := principleShortPath(f.Path)
			fmt.Fprintf(&sb, "  • %s: %s\n", short, f.Title)
		}
		sb.WriteString("\n")
	} else if len(invariantsFallback) > 0 {
		sb.WriteString("LOAD-BEARING INVARIANTS:\n")
		for _, f := range invariantsFallback {
			fmt.Fprintf(&sb, "  - %s\n    %s\n", f.Title, f.Path)
		}
		sb.WriteString("\n")
	}
	if len(recent) > 0 {
		sb.WriteString("Recent work in this repo:\n")
		for _, f := range recent {
			fmt.Fprintf(&sb, "  - %s: %s\n", f.Path, f.Title)
		}
	}
	if _, err := w.Write([]byte(sb.String())); err != nil {
		return err
	}
	emitted = true
	globalsCount = len(globals)
	invariantsCount = len(invariantsFallback)
	recentCount = len(recent)
	return nil
}

type factSummary struct {
	Path     string   `json:"path"`
	Title    string   `json:"title"`
	Entities []string `json:"entities"`
	Domain   []string `json:"domain"`
}

// sessionStartFactsURL builds the recent-facts URL the session-start hook
// uses. Pure function so a regression test can pin the exact shape (commit
// 99ec329 fixed a wrong-endpoint bug; keep this assertable).
func sessionStartFactsURL(repo, branch string) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/facts?sort=recent&limit=200",
		knomitBaseURL(), repo, encodeBranch(branch))
}

// fetchFacts calls the /facts HAL endpoint and returns the embedded
// facts collection. Returns nil on any error; each failure path logs at Warn
// so a dead server is visible in the bridge log rather than being
// indistinguishable from a legitimate empty result.
func fetchFacts(u string) []factSummary {
	resp, err := hookHTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("fetchFacts: GET failed")
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("fetchFacts: non-200")
		return nil
	}
	var body struct {
		Embedded struct {
			Facts []factSummary `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("fetchFacts: decode failed")
		return nil
	}
	return body.Embedded.Facts
}

// filterGlobalPrinciples returns facts under kb/principles/ whose Entities
// contain "designer" and Domain contains "global", up to max. The two-axis
// filter is what makes a principle "global": designer-authored AND scoped
// project-wide rather than to a single subarea.
func filterGlobalPrinciples(facts []factSummary, max int) []factSummary {
	out := make([]factSummary, 0, max)
	for _, f := range facts {
		if !strings.HasPrefix(f.Path, "kb/principles/") {
			continue
		}
		if !containsString(f.Entities, "designer") {
			continue
		}
		if !containsString(f.Domain, "global") {
			continue
		}
		out = append(out, f)
		if len(out) >= max {
			break
		}
	}
	return out
}

func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

// principleShortPath returns "<bucket>/<slug>" from a fact path like
// "kb/principles/<bucket>/<slug>/<uuid>.md". Falls back to the raw path
// on shape mismatch.
func principleShortPath(p string) string {
	const prefix = "kb/principles/"
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	rest := strings.TrimPrefix(p, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return rest
	}
	return parts[0] + "/" + parts[1]
}

func filterByPathPrefix(facts []factSummary, prefix string, max int) []factSummary {
	out := make([]factSummary, 0, max)
	for _, f := range facts {
		if strings.HasPrefix(f.Path, prefix) {
			out = append(out, f)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

func topN(facts []factSummary, n int) []factSummary {
	if len(facts) <= n {
		return facts
	}
	return facts[:n]
}
