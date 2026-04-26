// knomit-remote bridges stdio ↔ streamable-http for MCP.
//
// Claude Desktop (and other stdio-only MCP clients) launches this binary.
// It reads JSON-RPC messages from stdin, POSTs them to a knomit MCP
// endpoint, and writes responses back to stdout.
//
// Usage:
//
//	knomit-remote [--repo <name>] [--profile <profile>] [base-url]
//	knomit-remote
//	knomit-remote --repo work --profile chat
//	knomit-remote http://myhost:8080
//
// The base-url defaults to http://localhost:19278.
//
// Claude Desktop config:
//
//	{
//	  "mcpServers": {
//	    "knomit": {
//	      "command": "/path/to/knomit-remote"
//	    },
//	    "work-kb": {
//	      "command": "/path/to/knomit-remote",
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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var debug = os.Getenv("KNOMIT_MCP_DEBUG") != ""

func logDebug(format string, args ...any) {
	if debug {
		fmt.Fprintf(os.Stderr, "[knomit-remote] "+format+"\n", args...)
	}
}

func main() {
	repo := flag.String("repo", "knomit", "repository name")
	profile := flag.String("profile", "code", "MCP profile (code, chat, generic)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: knomit-remote [--repo <name>] [--profile <profile>] [base-url]\n")
		fmt.Fprintf(os.Stderr, "example: knomit-remote\n")
		fmt.Fprintf(os.Stderr, "         knomit-remote http://myhost:8080\n")
		fmt.Fprintf(os.Stderr, "         knomit-remote --repo work --profile chat\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	baseURL := "http://localhost:19278"
	if flag.NArg() >= 1 {
		baseURL = strings.TrimRight(flag.Arg(0), "/")
	} else if url, err := readLockfileBaseURL(); err == nil && url != "" {
		baseURL = url
		logDebug("discovered base-url from lockfile: %s", baseURL)
	} else if err != nil {
		logDebug("lockfile read failed, falling back to default: %v", err)
	}
	serverURL := fmt.Sprintf("%s/api/v1/%s/mcp?profile=%s", baseURL, *repo, *profile)
	logDebug("repo=%s profile=%s url=%s", *repo, *profile, serverURL)
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

		logDebug("← stdin: %s", truncate(line, 200))

		// Validate it's actual JSON before sending.
		if !json.Valid([]byte(line)) {
			logDebug("  skipping invalid JSON")
			continue
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

		resp, err := client.Do(req)
		if err != nil {
			writeError(os.Stdout, &mu, extractID(line), fmt.Sprintf("http request: %v", err))
			continue
		}

		logDebug("  → HTTP %d, Content-Type: %s", resp.StatusCode, resp.Header.Get("Content-Type"))

		// Capture session ID from initialize response.
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			sessionID = sid
			logDebug("  session: %s", sid)
		}

		if resp.StatusCode == http.StatusAccepted {
			// Notification accepted, no response body.
			resp.Body.Close()
			logDebug("  202 accepted (notification)")
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
				logDebug("  empty or invalid JSON response body")
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
	logDebug("→ stdout: %s", truncate(string(data), 200))
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
				writeLine(w, mu, []byte(data))
			} else if len(data) > 0 {
				logDebug("  SSE event=%q data=%s", eventType, truncate(data, 200))
			}
			continue
		}

		// Blank line = end of event, reset.
		if line == "" {
			eventType = ""
		}
	}
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
