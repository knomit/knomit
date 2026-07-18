package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// hookHTTPTimeout caps every hook-side HTTP call. Hooks run synchronously on
// every CC tool event; without a timeout, an unresponsive knomit server would
// hang CC indefinitely. The value is generous enough for warm local calls and
// short enough that a missing/dead server feels like a no-op.
const hookHTTPTimeout = 2 * time.Second

// hookHTTPClient is the shared client every hook uses. Reusing it allows
// connection-pool reuse across hooks within a session.
var hookHTTPClient = &http.Client{Timeout: hookHTTPTimeout}

// encodeBranch URL-encodes a branch name for inclusion in a knomit API path.
// Branches with slashes (e.g. "machine/host") are substituted "/" → ":" per
// the project convention; the server's branch-route handler does the reverse
// substitution. See kb/conventions/web/branch-slash-colon-substitution.
func encodeBranch(branch string) string {
	return strings.ReplaceAll(branch, "/", ":")
}

// knomitBaseURL returns the knomit HTTP base URL.
// Set KNOMIT_BASE_URL for non-default ports; otherwise the default works
// for a standard local install.
func knomitBaseURL() string {
	if u := os.Getenv("KNOMIT_BASE_URL"); u != "" {
		return u
	}
	return "http://localhost:19278"
}

// mcpBinding classifies the knomit MCP server config in .mcp.json under
// projectDir into either lens mode or repo mode. It is pure (no I/O beyond the
// single file read) so the classification is unit-testable in isolation.
//
// Returns (repo, lens):
//   - lens != "": lens mode — the file configures --lens <name>. repo is "".
//     A lens-configured file NEVER falls back to the basename; the caller must
//     resolve the write repo via the API and skip cleanly on failure.
//   - lens == "", repo != "": repo mode — the --repo <name> arg, or the
//     projectDir basename fallback when there is no readable/parseable knomit
//     config and no flag to read.
//
// Precedence when both flags appear (which `claude init` forbids, but a
// hand-edited .mcp.json could contain): --lens wins, regardless of argument
// order. A stray --repo must not demote a lens-configured session to a raw
// repo scope — lens mode resolves via the API and fails safe (skips) rather
// than risk reading the wrong repo.
func mcpBinding(projectDir string) (repo, lens string) {
	base := filepath.Base(projectDir)
	data, err := os.ReadFile(filepath.Join(projectDir, ".mcp.json"))
	if err != nil {
		return base, ""
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return base, ""
	}
	srv, ok := cfg.MCPServers["knomit"]
	if !ok {
		return base, ""
	}
	var repoArg string
	for i := 0; i < len(srv.Args); i++ {
		switch srv.Args[i] {
		case "--lens", "-lens":
			// A --lens token means lens mode even when the value is missing
			// (a hand-mangled config): lens wins and we NEVER fall back to the
			// basename. A degenerate --lens with no value yields an empty lens
			// name, which resolveWriteRepo/lensWriteRepo turn into a clean skip
			// rather than the wrong-repo hazard a basename fallback would
			// reintroduce.
			if i+1 < len(srv.Args) {
				return "", srv.Args[i+1]
			}
			return "", "" // lens flag with no value: lens mode, empty name
		case "--repo", "-repo":
			if repoArg == "" && i+1 < len(srv.Args) {
				repoArg = srv.Args[i+1]
			}
		}
	}
	if repoArg != "" {
		return repoArg, ""
	}
	return base, ""
}

// repoFromMCP returns the repo-mode target for projectDir (the --repo arg or
// the basename fallback). It is a thin wrapper over mcpBinding retained for the
// repo-mode call path and its regression tests; a lens-configured file yields
// "" here, so lens-aware callers must use resolveWriteRepo instead.
func repoFromMCP(projectDir string) string {
	repo, _ := mcpBinding(projectDir)
	return repo
}

// resolveWriteRepo maps a project directory to the knomit repo whose
// agent_branch and facts the hooks should read.
//
// Repo mode: returns the configured repo (or basename) with an empty skip
// reason — behavior is byte-identical to the pre-lens repoFromMCP path.
//
// Lens mode: resolves the lens's WRITE repo via GET /api/v1/lenses/{name}. On
// any error (server down, 404, decode) it returns ("", "lens_unresolved") so
// the hook skips cleanly — a lens-configured session NEVER falls back to the
// basename, which could name an unrelated repo and run the hook against the
// wrong data.
//
// Scope note: hook reads are deliberately write-repo-scoped. Until lens
// *browsing* REST exists (backlog A.1), the write repo is where the session's
// facts land, so session-start / post-edit context stays accurate for the
// write side.
func resolveWriteRepo(projectDir string) (repo, skipReason string) {
	r, lens := mcpBinding(projectDir)
	if r != "" {
		return r, "" // repo mode: configured --repo or basename fallback
	}
	// Lens mode: mcpBinding leaves repo empty. lens may itself be empty for a
	// hand-mangled --lens with no value; that resolves to "" and skips cleanly
	// below rather than falling back to the basename.
	w := lensWriteRepo(lens)
	if w == "" {
		return "", "lens_unresolved"
	}
	return w, ""
}

// lensWriteRepo queries knomit for a lens's write repo (the `write` field of
// the lens resource). Returns "" on any error — mirroring the graceful
// degradation of agentBranch / fetchFacts — with every failure path logged at
// Warn so a dead server is visible in the bridge log rather than silent.
func lensWriteRepo(name string) string {
	u := fmt.Sprintf("%s/api/v1/lenses/%s", knomitBaseURL(), name)
	resp, err := hookHTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("lensWriteRepo: GET failed")
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("lensWriteRepo: non-200")
		return ""
	}
	var body struct {
		Write string `json:"write"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("lensWriteRepo: decode failed")
		return ""
	}
	return body.Write
}

// agentBranch queries knomit for the repo's agent_branch. Returns "" on
// error so the caller can skip operations that need a branch. Every failure
// path emits a Warn log line so a misbehaving server is visible in the bridge
// log even though it stays silent toward CC.
func agentBranch(repo string) string {
	u := fmt.Sprintf("%s/api/v1/repos/%s", knomitBaseURL(), repo)
	resp, err := hookHTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("agentBranch: GET failed")
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("agentBranch: non-200")
		return ""
	}
	var body struct {
		AgentBranch string `json:"agent_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("agentBranch: decode failed")
		return ""
	}
	return body.AgentBranch
}

// emitAdditionalContext writes a JSON object to w that injects ctx as a
// system reminder via CC's hookSpecificOutput.additionalContext mechanism.
// Returns nil if ctx is empty (caller can short-circuit before any output).
func emitAdditionalContext(w io.Writer, ctx string) error {
	if ctx == "" {
		return nil
	}
	payload := struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}{}
	payload.HookSpecificOutput.AdditionalContext = ctx
	return json.NewEncoder(w).Encode(payload)
}
