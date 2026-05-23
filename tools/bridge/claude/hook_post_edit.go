package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"
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
	var (
		emitted      bool
		toolName     string
		factsMatched int
		skipReason   string
	)
	defer func() {
		ev := log.Info().Str("event", "post-edit").Bool("emitted", emitted)
		if toolName != "" {
			ev.Str("tool", toolName)
		}
		if emitted {
			ev.Int("facts_matched", factsMatched).Msg("hook result")
			return
		}
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		ev.Msg("hook result")
	}()

	var in postEditInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = "bad_input"
		return nil
	}
	toolName = in.ToolName
	switch in.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		skipReason = "non_edit_tool"
		return nil
	}

	rel := relPath(in.Cwd, in.ToolInput.FilePath)
	if rel == "" {
		skipReason = "path_outside_cwd"
		return nil
	}

	repo := repoFromMCP(in.Cwd)
	branch := agentBranch(repo)
	if branch == "" {
		skipReason = "no_agent_branch"
		return nil
	}

	// /search only accepts {q,limit,cursor} — `entities` is silently ignored
	// server-side. So we fuzzy-search by the path, then client-side filter
	// for facts whose `entities` array contains the exact rel path.
	candidates := fetchSearchResults(fmt.Sprintf(
		"%s/api/v1/repos/%s/branches/%s/search?q=%s&limit=20",
		knomitBaseURL(), repo, url.PathEscape(branch), url.QueryEscape(rel),
	))
	facts := filterByEntity(candidates, rel)
	if len(facts) == 0 {
		skipReason = "no_matching_facts"
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
	if err := emitAdditionalContext(w, sb.String()); err != nil {
		return err
	}
	emitted = true
	factsMatched = len(facts)
	return nil
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

// filterByEntity keeps only facts whose entities array contains the exact
// rel path. The search endpoint matches loosely on body/title/entities, so
// callers use it for retrieval; this is the precision step.
func filterByEntity(facts []factSummary, rel string) []factSummary {
	out := make([]factSummary, 0, len(facts))
	for _, f := range facts {
		if slices.Contains(f.Entities, rel) {
			out = append(out, f)
		}
	}
	return out
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
