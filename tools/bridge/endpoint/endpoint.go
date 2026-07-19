// Package endpoint discovers the knomit server's base URL and agent branch,
// and builds the profile-scoped MCP endpoint URL. It is shared by the
// stdio↔HTTP proxy (main.go) and the claw scaffolder, which both need to
// reach a live knomit server the same way.
package endpoint

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ServerURL builds the profile-scoped MCP endpoint. Branch path segments
// are separator-encoded (/ -> :) exactly as main.go did inline.
func ServerURL(base, repo, branch, profile string) string {
	encodedBranch := strings.ReplaceAll(branch, "/", ":")
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/mcp?profile=%s", base, repo, encodedBranch, profile)
}

// DiscoverAgentBranch queries GET /api/v1/repos/{repo} and returns the
// agent_branch field. This is the branch the local server writes facts to.
// Bounded by a short timeout so a missing/dead server fails fast at startup
// instead of hanging Claude Desktop.
func DiscoverAgentBranch(baseURL, repo string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/repos/%s", baseURL, repo)
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, body)
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

// ReadLockfileBaseURL returns http://127.0.0.1:<port> from the knomit-tray
// lockfile, or ("", nil) if the file does not exist.
func ReadLockfileBaseURL() (string, error) {
	path, err := LockfilePath()
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

func LockfilePath() (string, error) {
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
