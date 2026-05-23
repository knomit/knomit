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

// detectRequest mirrors the /detect endpoint's request body shape.
type detectRequest struct {
	Blocks         []detectBlock         `json:"blocks"`
	Intents        []string              `json:"intents"`
	NoveltyContext *detectNoveltyContext `json:"novelty_context,omitempty"`
}

type detectBlock struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type detectNoveltyContext struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

type detectResponse struct {
	Blocks []struct {
		Index   int `json:"index"`
		Signals []struct {
			Intent string  `json:"intent"`
			Score  float64 `json:"score"`
		} `json:"signals"`
		Novelty      *float64 `json:"novelty"`
		SimilarFacts []struct {
			Path       string  `json:"path"`
			Similarity float64 `json:"similarity"`
		} `json:"similar_facts"`
	} `json:"blocks"`
}

// postDetect calls /api/v1/profiles/code/detect with the given blocks and intents.
// novelty optional — pass nil to skip novelty context.
func postDetect(blocks []detectBlock, intents []string, novelty *detectNoveltyContext) (*detectResponse, error) {
	req := detectRequest{Blocks: blocks, Intents: intents, NoveltyContext: novelty}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := hookHTTPClient.Post( //nolint:noctx
		knomitBaseURL()+"/api/v1/profiles/code/detect",
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return nil, fmt.Errorf("post detect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("detect returned %d", resp.StatusCode)
	}
	var out detectResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode detect response: %w", err)
	}
	return &out, nil
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

// parseTranscript reads a CC transcript JSONL file and returns the last n
// user+assistant turns as detectBlocks. Defensive: skips lines that don't
// parse or lack expected fields. The exact CC transcript format isn't
// publicly documented, so this is best-effort and may need tuning.
func parseTranscript(path string, n int) ([]detectBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var blocks []detectBlock
	for _, line := range lines {
		var entry struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		text := extractMessageText(entry.Message)
		if text == "" {
			continue
		}
		blocks = append(blocks, detectBlock{Role: entry.Type, Text: text})
	}
	if len(blocks) > n {
		blocks = blocks[len(blocks)-n:]
	}
	return blocks, nil
}

// extractMessageText handles the message.content field's variable shape
// in CC transcripts. It can be a string OR an array of content parts.
func extractMessageText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return s
	}
	// Try array of parts {type, text}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}
