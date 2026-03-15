// knomit-mcp-remote bridges stdio ↔ streamable-http for MCP.
//
// Claude Desktop (and other stdio-only MCP clients) launches this binary.
// It reads JSON-RPC messages from stdin, POSTs them to a knomit MCP
// endpoint, and writes responses back to stdout.
//
// Usage:
//
//	knomit-mcp-remote http://localhost:3000/mcp?profile=chat
//
// Claude Desktop config:
//
//	{
//	  "mcpServers": {
//	    "knomit": {
//	      "command": "/path/to/knomit-mcp-remote",
//	      "args": ["http://localhost:3000/mcp?profile=chat"]
//	    }
//	  }
//	}
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: knomit-mcp-remote <url>\n")
		fmt.Fprintf(os.Stderr, "example: knomit-mcp-remote http://localhost:3000/mcp?profile=chat\n")
		os.Exit(1)
	}

	serverURL := os.Args[1]
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

		// Capture session ID from initialize response.
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			sessionID = sid
		}

		if resp.StatusCode == http.StatusAccepted {
			// Notification accepted, no response body.
			resp.Body.Close()
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
			mu.Lock()
			os.Stdout.Write(body)
			os.Stdout.Write([]byte("\n"))
			mu.Unlock()

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

// handleSSE reads an SSE stream and writes JSON-RPC messages to stdout.
// The MCP streamable-http spec sends SSE events with "event: message" and
// JSON-RPC data. Notifications may arrive before the final response.
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
			if eventType == "message" || eventType == "" {
				mu.Lock()
				fmt.Fprintln(w, data)
				mu.Unlock()
			}
			eventType = ""
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
	mu.Lock()
	fmt.Fprintln(w, string(data))
	mu.Unlock()
}
