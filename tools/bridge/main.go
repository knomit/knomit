// knomit-bridge bridges stdio ↔ streamable-http for MCP.
//
// Claude Desktop (and other stdio-only MCP clients) launches this binary.
// It reads JSON-RPC messages from stdin, POSTs them to a knomit MCP
// endpoint, and writes responses back to stdout.
//
// The MCP endpoint is branch-scoped:
//
//	/api/v1/repos/{repo}/branches/{branch}/mcp?profile={profile}
//
// knomit-bridge discovers the agent branch automatically by querying
// GET /api/v1/repos/{repo} and reading the agent_branch field.
//
// Usage:
//
//	knomit-bridge [--repo <name>] [--profile <profile>] [base-url]
//	knomit-bridge
//	knomit-bridge --repo work --profile chat
//	knomit-bridge http://myhost:8080
//
// The base-url defaults to http://localhost:19278.
//
// Claude Desktop config:
//
//	{
//	  "mcpServers": {
//	    "knomit": {
//	      "command": "/path/to/knomit-bridge"
//	    },
//	    "work-kb": {
//	      "command": "/path/to/knomit-bridge",
//	      "args": ["--repo", "work", "--profile", "chat"]
//	    }
//	  }
//	}
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/tools/bridge/bridgelog"
	"knomit/tools/bridge/claude"
	"knomit/tools/bridge/endpoint"
)

// peelLogFlag extracts --log / -log (with either '=value' or next-arg form)
// from args before any subcommand dispatch or flag.Parse, so the log path
// can be configured uniformly across the MCP and hook paths. Unknown args
// are passed through untouched.
func peelLogFlag(args []string) (logPath string, remaining []string) {
	logPath = bridgelog.DefaultPath
	remaining = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--log" || a == "-log":
			if i+1 < len(args) {
				logPath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--log=") || strings.HasPrefix(a, "-log="):
			_, v, _ := strings.Cut(a, "=")
			logPath = v
		default:
			remaining = append(remaining, a)
		}
	}
	return
}

func main() {
	logPath, args := peelLogFlag(os.Args[1:])
	bridgelog.Init(logPath)

	// Detect subcommands before flag.Parse() so we can handle them specially.
	if runVersion(args, os.Stdout) {
		return
	}
	if len(args) >= 1 && args[0] == "claude" {
		if err := claude.Run(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "knomit-bridge claude: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Re-seat os.Args for flag.Parse, minus the peeled --log entries.
	os.Args = append([]string{os.Args[0]}, args...)

	repo := flag.String("repo", config.DefaultRepoName, "repository name")
	source := flag.String("source", "", "source-code slug used in src:// refs (defaults to --repo)")
	profile := flag.String("profile", "code", "MCP profile (code, chat, generic)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: knomit-bridge [<command> [<subcommand>]] [flags] [base-url]\n\n")
		fmt.Fprintf(os.Stderr, "commands:\n")
		fmt.Fprintf(os.Stderr, "  claude init             Scaffold CC integration files in the current directory\n")
		fmt.Fprintf(os.Stderr, "                          knomit-bridge claude init [-repo <name>] [-source <slug>] [-profile <name>]\n\n")
		fmt.Fprintf(os.Stderr, "  claude hook <event>     Execute a Claude Code hook (called by CC via settings.json).\n")
		fmt.Fprintf(os.Stderr, "                          event in: session-start, post-edit, pre-compact\n\n")
		fmt.Fprintf(os.Stderr, "  version                 Print the build version and exit\n\n")
		fmt.Fprintf(os.Stderr, "without a command, runs as an MCP stdio↔HTTP proxy.\n\n")
		fmt.Fprintf(os.Stderr, "global flags (accepted before any subcommand):\n")
		fmt.Fprintf(os.Stderr, "  --log <path>            log file path (default %s, lumberjack 4MB rotation)\n\n", bridgelog.DefaultPath)
		fmt.Fprintf(os.Stderr, "examples:\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge http://myhost:8080\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge -repo work -source workapp -profile chat\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge --log /tmp/bridge.log claude hook post-edit\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge claude init -repo myproject\n")
		fmt.Fprintf(os.Stderr, "  knomit-bridge claude hook session-start  (typically run by CC, not interactively)\n")
		fmt.Fprintf(os.Stderr, "\nflags (for the default MCP-proxy mode):\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nNote: flags accept both '-flag value' and '--flag value' styles.\n")
	}
	flag.Parse()

	if *source == "" {
		// Backwards-compat: prior releases used .mcp.json entries without
		// --source. Default to --repo and warn rather than fail; otherwise
		// every existing user's Claude Desktop config breaks on upgrade.
		*source = *repo
		log.Warn().Str("repo", *repo).Msg("--source not set; defaulting to --repo. Add --source explicitly in .mcp.json to silence this.")
	}

	fmt.Fprintf(os.Stderr, "[knomit-bridge] log file: %s (pid=%d)\n", logPath, os.Getpid())
	log.Info().Str("repo", *repo).Str("source", *source).Str("profile", *profile).Msg("bridge starting")

	baseURL := "http://localhost:19278"
	if flag.NArg() >= 1 {
		baseURL = strings.TrimRight(flag.Arg(0), "/")
	} else if url, err := endpoint.ReadLockfileBaseURL(); err == nil && url != "" {
		baseURL = url
		log.Debug().Str("base_url", baseURL).Msg("discovered base-url from lockfile")
	} else if err != nil {
		log.Debug().Err(err).Msg("lockfile read failed, falling back to default")
	}
	branch, err := endpoint.DiscoverAgentBranch(baseURL, *repo)
	if err != nil {
		log.Error().Err(err).Str("repo", *repo).Msg("failed to discover agent branch")
		fmt.Fprintf(os.Stderr, "knomit-bridge: failed to discover agent branch for repo %q: %v\n", *repo, err)
		os.Exit(1)
	}
	serverURL := endpoint.ServerURL(baseURL, *repo, branch, *profile)
	log.Info().Str("repo", *repo).Str("branch", branch).Str("profile", *profile).Str("url", serverURL).Msg("bridge configured")
	client := &http.Client{}

	var (
		sessionID string
		mu        sync.Mutex // protects stdout writes
	)

	scanner := bufio.NewScanner(os.Stdin)
	// Allow large messages (16 MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		log.Debug().Str("line", truncate(line, 200)).Msg("← stdin")

		// Validate it's actual JSON before sending.
		if !json.Valid([]byte(line)) {
			log.Debug().Msg("skipping invalid JSON")
			continue
		}

		method, toolName := extractMethod(line)
		label := method
		if toolName != "" {
			label = method + " " + toolName
		}

		req, err := http.NewRequest(http.MethodPost, serverURL, bytes.NewReader([]byte(line)))
		if err != nil {
			writeError(os.Stdout, &mu, nil, fmt.Sprintf("create request: %v", err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}

		reqStart := time.Now()
		log.Info().Str("label", label).Msg("→ http")
		resp, err := client.Do(req)
		elapsed := time.Since(reqStart)
		if err != nil {
			log.Warn().Err(err).Str("label", label).Dur("elapsed", elapsed).Msg("← http error")
			writeError(os.Stdout, &mu, extractID(line), fmt.Sprintf("http request: %v", err))
			continue
		}

		log.Info().Str("label", label).Int("status", resp.StatusCode).Dur("elapsed", elapsed).Str("content_type", resp.Header.Get("Content-Type")).Msg("← http response headers")

		// Capture session ID from initialize response.
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			sessionID = sid
			log.Debug().Str("session", sid).Msg("captured session id")
		}

		if resp.StatusCode == http.StatusAccepted {
			// Notification accepted, no response body.
			resp.Body.Close()
			log.Debug().Msg("202 accepted (notification)")
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			writeError(os.Stdout, &mu, extractID(line), fmt.Sprintf("server error %d: %s", resp.StatusCode, body))
			continue
		}

		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		switch mediaType {
		case "application/json":
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) > 0 && json.Valid(body) {
				writeLine(os.Stdout, &mu, body)
			} else {
				log.Debug().Msg("empty or invalid JSON response body")
			}

		case "text/event-stream":
			handleSSE(resp.Body, os.Stdout, &mu)
			resp.Body.Close()

		default:
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			writeError(os.Stdout, &mu, extractID(line), fmt.Sprintf("unexpected content-type %q: %s", mediaType, body))
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stdin read error: %v\n", err)
		os.Exit(1)
	}
}

// writeLine writes a JSON line to stdout as a single atomic write,
// ensuring exactly one trailing newline.
func writeLine(w io.Writer, mu *sync.Mutex, data []byte) {
	// Trim any trailing whitespace from the data to avoid double newlines.
	data = bytes.TrimRight(data, " \t\r\n")
	if len(data) == 0 {
		return
	}
	log.Debug().Str("data", truncate(string(data), 200)).Msg("→ stdout")
	// Single write call to avoid partial reads on the pipe.
	msg := make([]byte, len(data)+1)
	copy(msg, data)
	msg[len(data)] = '\n'
	mu.Lock()
	w.Write(msg)
	mu.Unlock()
}

// handleSSE reads an SSE stream and writes JSON-RPC messages to stdout.
// Per the MCP streamable-http spec, JSON-RPC messages arrive as SSE events
// with "event: message" and a "data:" line containing the JSON-RPC payload.
func handleSSE(r io.Reader, w io.Writer, mu *sync.Mutex) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var eventType string
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			// Only forward "message" events with valid JSON content.
			if eventType == "message" && len(data) > 0 && json.Valid([]byte(data)) {
				// Log notifications (have method, no id) so SSE delivery is
				// visible at info level without enabling debug.
				var envelope struct {
					Method string          `json:"method"`
					ID     json.RawMessage `json:"id"`
				}
				if json.Unmarshal([]byte(data), &envelope) == nil && envelope.Method != "" && envelope.ID == nil {
					log.Info().Str("method", envelope.Method).Msg("← SSE notification")
				}
				writeLine(w, mu, []byte(data))
			} else if len(data) > 0 {
				log.Debug().Str("event", eventType).Str("data", truncate(data, 200)).Msg("SSE event")
			}
			continue
		}

		// Blank line = end of event, reset.
		if line == "" {
			eventType = ""
		}
	}
}

// extractMethod returns the JSON-RPC method and, for tools/call, the tool name.
func extractMethod(line string) (method, toolName string) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &msg) == nil {
		method = msg.Method
		if method == "tools/call" {
			toolName = msg.Params.Name
		}
	}
	return method, toolName
}

// extractID pulls the "id" field from a JSON-RPC message for error responses.
func extractID(line string) json.RawMessage {
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal([]byte(line), &msg) == nil && msg.ID != nil {
		return msg.ID
	}
	return nil
}

// writeError writes a JSON-RPC error response to stdout.
func writeError(w io.Writer, mu *sync.Mutex, id json.RawMessage, msg string) {
	if id == nil {
		// Can't send an error without a request ID — log to stderr instead.
		fmt.Fprintf(os.Stderr, "error (no id): %s\n", msg)
		return
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": msg},
	}
	data, _ := json.Marshal(resp)
	writeLine(w, mu, data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
