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
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// httpTimeout caps every hook-side HTTP call. Hooks run synchronously on every
// agent tool event; without a timeout, an unresponsive knomit server would hang
// the agent indefinitely. Generous enough for warm local calls, short enough
// that a missing/dead server feels like a no-op.
const httpTimeout = 2 * time.Second

// hooksClient is built on FIRST USE, never at package init, and reached only
// through Client(). It prefers the unix socket for the same reason the proxy
// client does — the kernel vouches for the caller — unless KNOMIT_BASE_URL
// names a server, in which case the operator has chosen one and that choice
// stands. The hooks have no CLI argument, so that env var is the only
// explicit form here.
var (
	hooksOnce   sync.Once
	hooksClient *http.Client
)

// Client is the shared hooks client, so hooks within a session reuse the
// connection pool.
//
// It is a FUNCTION and not a package-level var on purpose. A var would be
// initialised at package-init time, before any caller — or any test's
// t.Setenv — could say where the server is, and the transport chosen from
// whatever ambient state the machine happened to be in would then be frozen
// for the life of the process. That made this package's own suite pass or
// fail on whether a socket existed at the developer's ~/.knomit, which is a
// green run that says nothing about the commit.
//
// Laziness alone would only move that freeze later, so the socket decision is
// ALSO made per dial (see socketPreferringClient): this function may be
// called once, and the answer still tracks the environment afterwards.
func Client() *http.Client {
	hooksOnce.Do(func() { hooksClient = newLazyHooksClient(httpTimeout) })
	return hooksClient
}

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
	resp, err := Client().Get(u) //nolint:noctx
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
	resp, err := Client().Get(u) //nolint:noctx
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
