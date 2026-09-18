// knomit-bridge bridges stdio ↔ streamable-http for MCP.
//
// Claude Desktop (and other stdio-only MCP clients) launches this binary.
// It reads JSON-RPC messages from stdin, POSTs them to a knomit MCP
// endpoint, and writes responses back to stdout.
//
// The MCP endpoint is branch-scoped:
//
//	/api/v1/repos/{repo}/branches/{branch}/mcp
//
// or, with neither --repo nor --lens, the session-bound mount:
//
//	/api/v1/mcp
//
// knomit-bridge discovers the agent branch automatically by querying
// GET /api/v1/repos/{repo} and reading the agent_branch field.
//
// Usage:
//
//	kb --repo <name> [base-url]
//	kb --lens <name> [base-url]
//	kb [base-url]
//	kb --repo work http://myhost:8080
//
// --repo and --lens are mutually exclusive. With neither, the bridge connects
// to the unscoped mount /api/v1/mcp, where the agent calls knomit_bind and
// passes the handle it returns on every other tool call; knomit has no default
// repo either way.
// The base-url defaults to http://localhost:19278.
//
// Claude Desktop config:
//
//	{
//	  "mcpServers": {
//	    "work-kb": {
//	      "command": "/path/to/kb",
//	      "args": ["--repo", "work"]
//	    }
//	  }
//	}
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/tools/bridge/antigravity"
	"knomit/tools/bridge/bridgelog"
	"knomit/tools/bridge/claude"
)

// peelLogFlag extracts --log / -log (with either '=value' or next-arg form)
// from args before any subcommand dispatch or flag.Parse, so the log path
// can be configured uniformly across the MCP and hook paths. Unknown args
// are passed through untouched.
func peelLogFlag(args []string) (logPath string, remaining []string) {
	logPath = bridgelog.DefaultPath()
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

// lensConflict returns the mutual-exclusion message when --lens is combined
// with an explicitly-set --repo, or "" when there is no conflict. The caller
// passes whether --repo was explicitly set (via flag.Visit) rather than
// comparing values, so that an explicit `--repo ""` still reads as a conflict.
func lensConflict(lens string, repoSet bool) string {
	if lens == "" {
		return ""
	}
	if repoSet {
		return "--lens and --repo are mutually exclusive"
	}
	return ""
}

// bridgeMode is which endpoint the proxy connects to.
type bridgeMode int

const (
	modeRepo bridgeMode = iota
	modeLens
	modeSessionBound
	modeInvalid
)

// selectMode decides the proxy mode from the flag VALUES plus whether each flag
// was explicitly given (flag.Visit), which are different questions.
//
// Session-bound mode is the absence of BOTH flags — never two empty strings. An
// explicit `--repo ""` is a misconfigured wrapper (an unset variable that
// expanded to nothing), and silently proxying it to the session-bound mount
// would turn that mistake into a working-but-wrong session bound to whatever
// the agent later picks. It stays a hard exit, as it was before the mount
// existed. lensConflict cannot catch it: it short-circuits on lens == "".
func selectMode(repo, lens string, repoSet, lensSet bool) bridgeMode {
	switch {
	case !repoSet && !lensSet:
		return modeSessionBound
	case lens != "":
		return modeLens
	case repo != "":
		return modeRepo
	default:
		// A flag was given but empty.
		return modeInvalid
	}
}

// baseURLArg validates the optional leading positional argument as the server
// base URL, returning "" when there is none.
//
// This guard exists because session-bound mode made no-flags legal. Before it,
// a mistyped subcommand still failed loudly: `kb clade init` parsed
// no --repo and the required-flag check exited. Now the same typo would be
// accepted as a base URL and the proxy would dial http://clade/... forever.
//
// Go's flag package stops parsing at the FIRST non-flag argument, so
// `clade init -repo x` never parses -repo at all: "clade", "init", "-repo" and
// "x" all land in flag.Args(), flag.Visit reports neither flag as set, and the
// mode selector picks session-bound. Rejecting a first positional that is not
// an http/https URL is what turns that silent misconfiguration back into an
// exit.
func baseURLArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	raw := args[0]
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("unknown command or base-url %q (expected http:// or https://)", raw)
	}
	return strings.TrimRight(raw, "/"), nil
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
	if len(args) >= 1 && (args[0] == "antigravity" || args[0] == "agy") {
		if err := antigravity.Run(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "knomit-bridge antigravity: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Re-seat os.Args for flag.Parse, minus the peeled --log entries.
	os.Args = append([]string{os.Args[0]}, args...)

	// No default: knomit serves no privileged repo, so the bridge cannot guess
	// which one to proxy. Give --repo, or --lens, or neither — with neither the
	// bridge connects to the unscoped mount and the agent binds per call.
	repo := flag.String("repo", "", "repository name (omit both --repo and --lens to let the agent bind via knomit_bind)")
	lens := flag.String("lens", "", "lens name; connects to /api/v1/lenses/<lens>/mcp (mutually exclusive with --repo)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kb [<command> [<subcommand>]] [flags] [base-url]\n\n")
		fmt.Fprintf(os.Stderr, "commands:\n")
		fmt.Fprintf(os.Stderr, "  claude init             Scaffold CC integration files in the current directory\n")
		fmt.Fprintf(os.Stderr, "                          kb claude init [-repo <name>]\n\n")
		fmt.Fprintf(os.Stderr, "  claude hook <event>     Execute a Claude Code hook (called by CC via settings.json).\n")
		fmt.Fprintf(os.Stderr, "                          event in: session-start, post-edit, post-ask, pre-compact\n\n")
		fmt.Fprintf(os.Stderr, "  antigravity init        Scaffold the Antigravity plugin in the current directory\n")
		fmt.Fprintf(os.Stderr, "                          kb antigravity init [-repo <name>|-lens <name>]\n")
		fmt.Fprintf(os.Stderr, "                          (alias: agy)\n\n")
		fmt.Fprintf(os.Stderr, "  antigravity hook <event>  Execute an Antigravity hook (called by agy via hooks.json).\n")
		fmt.Fprintf(os.Stderr, "                          event in: pre-invocation\n\n")
		fmt.Fprintf(os.Stderr, "  version                 Print the build version and exit\n\n")
		fmt.Fprintf(os.Stderr, "without a command, runs as an MCP stdio↔HTTP proxy.\n\n")
		fmt.Fprintf(os.Stderr, "global flags (accepted before any subcommand):\n")
		fmt.Fprintf(os.Stderr, "  --log <path>            log file path (default %s, lumberjack 4MB rotation)\n\n", bridgelog.DefaultPath())
		fmt.Fprintf(os.Stderr, "examples:\n")
		fmt.Fprintf(os.Stderr, "  kb -repo work\n")
		fmt.Fprintf(os.Stderr, "  kb -lens eng\n")
		fmt.Fprintf(os.Stderr, "  kb                                       (unscoped: the agent calls knomit_bind)\n")
		fmt.Fprintf(os.Stderr, "  kb -repo work http://myhost:8080\n")
		fmt.Fprintf(os.Stderr, "  kb --log /tmp/bridge.log claude hook post-edit\n")
		fmt.Fprintf(os.Stderr, "  kb claude init -repo myproject\n")
		fmt.Fprintf(os.Stderr, "  kb claude hook session-start             (typically run by CC, not interactively)\n")
		fmt.Fprintf(os.Stderr, "  kb antigravity init -repo myproject\n")
		fmt.Fprintf(os.Stderr, "\nflags (for the default MCP-proxy mode):\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nNote: flags accept both '-flag value' and '--flag value' styles.\n")
	}
	flag.Parse()

	// --lens is mutually exclusive with --repo. Omitting BOTH selects the
	// session-bound mount; giving one but leaving it empty is a
	// misconfiguration, not a request for that mount. flag.Visit (rather than
	// *repo != "") is what distinguishes "not given" from "given empty".
	repoSet, lensSet := false, false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "repo":
			repoSet = true
		case "lens":
			lensSet = true
		}
	})
	if msg := lensConflict(*lens, repoSet); msg != "" {
		log.Fatal().Msg(msg)
	}
	mode := selectMode(*repo, *lens, repoSet, lensSet)
	if mode == modeInvalid {
		fmt.Fprintf(os.Stderr,
			"knomit-bridge: --repo/--lens given but empty; omit both to let the agent bind via knomit_bind\n")
		flag.Usage()
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "[knomit-bridge] log file: %s (pid=%d)\n", logPath, os.Getpid())
	log.Info().Str("repo", *repo).Msg("bridge starting")

	baseURL := "http://localhost:19278"
	arg, argErr := baseURLArg(flag.Args())
	if argErr != nil {
		fmt.Fprintf(os.Stderr, "knomit-bridge: %v\n", argErr)
		flag.Usage()
		os.Exit(2)
	}
	if arg != "" {
		baseURL = arg
	} else if lockURL, err := readLockfileBaseURL(); err == nil && lockURL != "" {
		baseURL = lockURL
		log.Debug().Str("base_url", baseURL).Msg("discovered base-url from lockfile")
	} else if err != nil {
		log.Debug().Err(err).Msg("lockfile read failed, falling back to default")
	}
	var serverURL string
	// branch is also what the bridge declares about itself; it stays empty in
	// lens mode, where the branch is resolved per mount server-side.
	var branch string
	if mode == modeSessionBound {
		// Unscoped mode: nothing to discover. The mount names no repo, and the
		// agent binds by calling knomit_bind and passing the handle it returns
		// on every other call; until it does, every other tool fails.
		//
		// Note this is a property of the BRIDGE's connection, not of any
		// session: several independent jobs may share one bridge process, and
		// each holds its own handle. The bridge itself stays ignorant — it
		// forwards tool arguments untouched.
		serverURL = mcpURL(baseURL, "", "", "")
		log.Info().Str("url", serverURL).Msg("bridge configured (unscoped; the agent calls knomit_bind)")
	} else if mode == modeLens {
		// Lens mode: skip branch discovery entirely. A lens resolves each
		// mount's branch server-side via LensMiddleware, so the bridge just
		// connects to the lens endpoint (no branch).
		serverURL = mcpURL(baseURL, "", *lens, "")
		log.Info().Str("lens", *lens).Str("url", serverURL).Msg("bridge configured (lens)")
	} else {
		var err error
		branch, err = discoverAgentBranch(baseURL, *repo)
		if err != nil {
			log.Error().Err(err).Str("repo", *repo).Msg("failed to discover agent branch")
			fmt.Fprintf(os.Stderr, "knomit-bridge: failed to discover agent branch for repo %q: %v\n", *repo, err)
			os.Exit(1)
		}
		encodedBranch := strings.ReplaceAll(branch, "/", ":")
		serverURL = mcpURL(baseURL, *repo, "", encodedBranch)
		log.Info().Str("repo", *repo).Str("branch", branch).Str("url", serverURL).Msg("bridge configured")
	}

	// Identity is computed once and never re-read: this process is one
	// instance for its whole life.
	hdr := clientHeaders(buildIdentity(branch, time.Now()))
	client := &http.Client{}

	sessionID, err := runProxy(os.Stdin, os.Stdout, client, serverURL, hdr)
	// stdin closed: the host is gone. Tell the server so the row is marked
	// ended instead of going dead by silence. Fire-and-forget.
	terminateSession(client, serverURL, sessionID, hdr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stdin read error: %v\n", err)
		os.Exit(1)
	}
}

// runProxy is the stdio↔HTTP loop: one JSON-RPC line in, one POST out, the
// response (JSON or SSE) written back as lines. hdr is copied onto every
// request. Returns the Mcp-Session-Id captured from the initialize response
// — the bridge sends it on every later request for its whole life — and the
// scanner error, if any. There is NO reconnect, retry or recovery here by
// design (kb/decisions/integrations/bridge/no-recovery).
func runProxy(in io.Reader, out io.Writer, client *http.Client, serverURL string, hdr http.Header) (sessionID string, err error) {
	var mu sync.Mutex // protects out writes

	scanner := bufio.NewScanner(in)
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

		req, rerr := http.NewRequest(http.MethodPost, serverURL, bytes.NewReader([]byte(line)))
		if rerr != nil {
			writeError(out, &mu, nil, fmt.Sprintf("create request: %v", rerr))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range hdr {
			req.Header[k] = v
		}
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}

		reqStart := time.Now()
		log.Info().Str("label", label).Msg("→ http")
		resp, derr := client.Do(req)
		elapsed := time.Since(reqStart)
		if derr != nil {
			log.Warn().Err(derr).Str("label", label).Dur("elapsed", elapsed).Msg("← http error")
			writeError(out, &mu, extractID(line), fmt.Sprintf("http request: %v", derr))
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
			writeError(out, &mu, extractID(line), fmt.Sprintf("server error %d: %s", resp.StatusCode, body))
			continue
		}

		mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		switch mediaType {
		case "application/json":
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(body) > 0 && json.Valid(body) {
				writeLine(out, &mu, body)
			} else {
				log.Debug().Msg("empty or invalid JSON response body")
			}

		case "text/event-stream":
			handleSSE(resp.Body, out, &mu)
			resp.Body.Close()

		default:
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			writeError(out, &mu, extractID(line), fmt.Sprintf("unexpected content-type %q: %s", mediaType, body))
		}
	}

	return sessionID, scanner.Err()
}

// terminateSession sends the MCP session-termination DELETE. Best effort with
// a short deadline: a dead or relocated server (the self-update orphan case)
// must not delay the exit, and the row then goes dead by silence instead.
func terminateSession(client *http.Client, serverURL, sessionID string, hdr http.Header) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, serverURL, nil)
	if err != nil {
		return
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := client.Do(req)
	if err != nil {
		log.Debug().Err(err).Msg("session terminate: server unreachable (expected after a restart)")
		return
	}
	resp.Body.Close()
	log.Info().Int("status", resp.StatusCode).Msg("session terminated")
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

// mcpURL builds the MCP endpoint URL for a repo (branch-scoped) or a lens.
// A lens has no branch segment — LensMiddleware resolves each mount's branch
// server-side.
func mcpURL(baseURL, repo, lens, encodedBranch string) string {
	// Neither set: the unscoped mount. The agent chooses its repo or lens with
	// knomit_bind and names it per call by handle, so the URL names none.
	if repo == "" && lens == "" {
		return baseURL + "/api/v1/mcp"
	}
	if lens != "" {
		return fmt.Sprintf("%s/api/v1/lenses/%s/mcp", baseURL, lens)
	}
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/mcp", baseURL, repo, encodedBranch)
}

// discoverAgentBranch queries GET /api/v1/repos/{repo} and returns the
// agent_branch field. This is the branch the local server writes facts to.
// Bounded by a short timeout so a missing/dead server fails fast at startup
// instead of hanging Claude Desktop.
func discoverAgentBranch(baseURL, repo string) (string, error) {
	repoURL := fmt.Sprintf("%s/api/v1/repos/%s", baseURL, repo)
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(repoURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", repoURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s: status %d: %s", repoURL, resp.StatusCode, body)
	}
	var body struct {
		AgentBranch string `json:"agent_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode repo response: %w", err)
	}
	if body.AgentBranch == "" {
		return "", fmt.Errorf("server did not return agent_branch for repo %q", repo)
	}
	return body.AgentBranch, nil
}

// readLockfileBaseURL returns http://127.0.0.1:<port> from the knomit-tray
// lockfile, or ("", nil) if the file does not exist.
func readLockfileBaseURL() (string, error) {
	path, err := lockfilePath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var info struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("parse lockfile %s: %w", path, err)
	}
	if info.Port <= 0 {
		return "", nil
	}
	return fmt.Sprintf("http://127.0.0.1:%d", info.Port), nil
}

func lockfilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "knomit", "server.json"), nil
	case "linux":
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "knomit", "server.json"), nil
		}
		return filepath.Join(home, ".local", "state", "knomit", "server.json"), nil
	}
	return "", fmt.Errorf("lockfile path: unsupported platform %s", runtime.GOOS)
}
