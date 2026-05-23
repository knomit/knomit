package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type sessionStartInput struct {
	Cwd            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// hookSessionStart fires once per CC session. CC auto-wraps plain stdout
// from this hook as a system reminder, so we emit plain text (no JSON envelope).
func hookSessionStart(r io.Reader, w io.Writer) error {
	var in sessionStartInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil // exit cleanly on bad input — don't disrupt session start
	}
	repo := repoFromMCP(in.Cwd)
	branch := agentBranch(repo)
	if branch == "" {
		return nil
	}

	base := knomitBaseURL()
	invariants := fetchFactList(fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/search?path=%s",
		base, repo, url.PathEscape(branch), url.QueryEscape("invariants/")))
	recent := fetchFactList(fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/activity?limit=5",
		base, repo, url.PathEscape(branch)))

	if len(invariants) == 0 && len(recent) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("Known facts from knomit for this codebase:\n\n")
	if len(invariants) > 0 {
		sb.WriteString("LOAD-BEARING INVARIANTS:\n")
		for _, f := range invariants {
			fmt.Fprintf(&sb, "  - %s\n    %s\n", f.Title, f.Body)
		}
		sb.WriteString("\n")
	}
	if len(recent) > 0 {
		sb.WriteString("Recent work in this repo:\n")
		for _, f := range recent {
			fmt.Fprintf(&sb, "  - %s: %s\n", f.Path, f.Title)
		}
	}
	_, err := w.Write([]byte(sb.String()))
	return err
}

type factSummary struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func fetchFactList(u string) []factSummary {
	resp, err := http.Get(u) //nolint:noctx
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	// Response is HAL: {"_embedded": {"facts": [...]}}
	var body struct {
		Embedded struct {
			Facts []factSummary `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	return body.Embedded.Facts
}
