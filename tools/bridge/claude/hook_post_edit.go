package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

type postEditInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// hookPostEdit fires after Edit / Write / MultiEdit tool calls. When the
// edited file is referenced as an entity by one or more knomit facts, it
// nudges CC to /knomit-update or /knomit-retract any that drifted.
//
// Best-effort: matches by the `entities` field on facts (which conventionally
// holds relative source paths). Misses facts that mention the file only in
// `refs` or `body`. A future iteration can add a server-side endpoint that
// searches refs literally; until then, the entities match is the strongest
// no-server-change signal available.
func hookPostEdit(r io.Reader, w io.Writer) error {
	var in postEditInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil
	}
	switch in.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return nil
	}

	rel := relPath(in.Cwd, in.ToolInput.FilePath)
	if rel == "" {
		return nil
	}

	repo := repoFromMCP(in.Cwd)
	branch := agentBranch(repo)
	if branch == "" {
		return nil
	}

	facts := fetchSearchResults(fmt.Sprintf(
		"%s/api/v1/repos/%s/branches/%s/search?entities=%s&limit=10",
		knomitBaseURL(), repo, url.PathEscape(branch), url.QueryEscape(rel),
	))
	if len(facts) == 0 {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "You just edited %s. %d knomit fact(s) reference this file as an entity and may now be stale:\n\n",
		rel, len(facts))
	for _, f := range facts {
		fmt.Fprintf(&sb, "  - %s — %s\n", f.Path, f.Title)
	}
	sb.WriteString("\nFor each, decide:\n")
	sb.WriteString("  - Still accurate? do nothing\n")
	sb.WriteString("  - Drift in body/confidence/refs? `/knomit-update <path>`\n")
	sb.WriteString("  - Wholly wrong or subject no longer exists? `/knomit-retract <path>`\n")
	return emitAdditionalContext(w, sb.String())
}

// relPath returns the path of abs relative to cwd, or "" if abs is outside
// cwd. Both must be absolute paths in the same filesystem.
func relPath(cwd, abs string) string {
	if cwd == "" || abs == "" {
		return ""
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return ""
	}
	if strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}

// fetchSearchResults calls a /search HAL endpoint and returns the embedded
// results collection. Returns nil on any error (server down, bad response,
// etc.) — hooks must never fail loudly.
func fetchSearchResults(u string) []factSummary {
	resp, err := http.Get(u) //nolint:noctx
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Embedded struct {
			Results []factSummary `json:"results"`
		} `json:"_embedded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	return body.Embedded.Results
}
