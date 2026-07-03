// Package claw scaffolds the OpenClaw plugin integration for knomit-bridge.
package claw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// SnapshotTools performs a minimal MCP handshake against the profile-scoped
// /mcp endpoint (initialize, then tools/list) and returns the compact
// knomit-tools.json the OpenClaw plugin loads at registration time.
//
// The server issues an Mcp-Session-Id on the initialize response and
// requires it on subsequent requests (see main.go's stdio↔HTTP proxy loop),
// so it is captured here and threaded into the tools/list call.
func SnapshotTools(serverURL string, httpClient *http.Client) ([]byte, error) {
	var sessionID string
	if _, err := rpc(httpClient, serverURL, &sessionID, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "knomit-claw-init", "version": "1"},
	}); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	res, err := rpc(httpClient, serverURL, &sessionID, 2, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var parsed struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	return json.MarshalIndent(parsed.Tools, "", "  ")
}

// rpc issues one JSON-RPC request. sessionID is read (if non-empty, sent as
// the Mcp-Session-Id header) and written (updated from the response header,
// if present) so callers can thread session state across a handshake.
func rpc(c *http.Client, url string, sessionID *string, id int, method string, params any) (json.RawMessage, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != nil && *sessionID != "" {
		req.Header.Set("Mcp-Session-Id", *sessionID)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" && sessionID != nil {
		*sessionID = sid
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", env.Error.Message)
	}
	return env.Result, nil
}
