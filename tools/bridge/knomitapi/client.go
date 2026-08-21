// Package knomitapi is the host-neutral client for a local knomit server.
// Both the Claude Code and Antigravity bridge hosts read facts through it, so
// nothing here may know which agent is calling.
package knomitapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// httpTimeout caps every hook-side HTTP call. Hooks run synchronously on every
// agent tool event; without a timeout, an unresponsive knomit server would hang
// the agent indefinitely. Generous enough for warm local calls, short enough
// that a missing/dead server feels like a no-op.
const httpTimeout = 2 * time.Second

// HTTPClient is shared so hooks within a session reuse the connection pool.
var HTTPClient = &http.Client{Timeout: httpTimeout}

// EncodeBranch URL-encodes a branch name for a knomit API path. Branches with
// slashes (e.g. "machine/host") are substituted "/" -> ":" per the project
// convention; the server's branch-route handler does the reverse.
// See kb/conventions/web/branch-slash-colon-substitution.
func EncodeBranch(branch string) string {
	return strings.ReplaceAll(branch, "/", ":")
}

// BaseURL returns the knomit HTTP base URL. Set KNOMIT_BASE_URL for
// non-default ports; otherwise the default works for a standard local install.
func BaseURL() string {
	if u := os.Getenv("KNOMIT_BASE_URL"); u != "" {
		return u
	}
	return "http://localhost:19278"
}

// AgentBranch returns the repo's agent_branch, or "" on any error so the
// caller can skip operations that need a branch. Every failure path logs at
// Warn so a misbehaving server is visible in the bridge log even though the
// hook stays silent toward the agent.
func AgentBranch(repo string) string {
	u := fmt.Sprintf("%s/api/v1/repos/%s", BaseURL(), url.PathEscape(repo))
	resp, err := HTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("AgentBranch: GET failed")
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("AgentBranch: non-200")
		return ""
	}
	var body struct {
		AgentBranch string `json:"agent_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("AgentBranch: decode failed")
		return ""
	}
	return body.AgentBranch
}

// LensWriteRepo returns a lens's write repo NAME. The lens resource names each
// member as a {uid, name} pair: uid is the registry key membership is stored
// under, name is the server-resolved display name. Callers feed the result to
// AgentBranch, and /api/v1/repos/{repo} is name-addressed — handing it a uid
// would 404 on every lens-mode hook.
//
// Returns "" on any error, with every failure path logged at Warn.
func LensWriteRepo(name string) string {
	u := fmt.Sprintf("%s/api/v1/lenses/%s", BaseURL(), url.PathEscape(name))
	resp, err := HTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("LensWriteRepo: GET failed")
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("LensWriteRepo: non-200")
		return ""
	}
	var body struct {
		Write struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"write"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("LensWriteRepo: decode failed")
		return ""
	}
	return body.Write.Name
}
