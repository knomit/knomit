package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// knomitBaseURL returns the knomit HTTP base URL.
// Set KNOMIT_BASE_URL for non-default ports; otherwise the default works
// for a standard local install.
func knomitBaseURL() string {
	if u := os.Getenv("KNOMIT_BASE_URL"); u != "" {
		return u
	}
	return "http://localhost:19278"
}

// repoFromMCP reads .mcp.json under projectDir and returns the configured
// repo name (the --repo arg). Falls back to projectDir's basename.
func repoFromMCP(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, ".mcp.json"))
	if err != nil {
		return filepath.Base(projectDir)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return filepath.Base(projectDir)
	}
	srv, ok := cfg.MCPServers["knomit"]
	if !ok {
		return filepath.Base(projectDir)
	}
	for i := 0; i+1 < len(srv.Args); i++ {
		if srv.Args[i] == "--repo" || srv.Args[i] == "-repo" {
			return srv.Args[i+1]
		}
	}
	return filepath.Base(projectDir)
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
