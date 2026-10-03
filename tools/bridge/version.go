package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/platform/version"
)

// runVersion handles the `knomit-bridge version` subcommand. It reports
// whether it consumed the args; when it did, it has already written the build
// version to out. Mirrors the early-dispatch style of the `claude` subcommand.
//
// It stays OFFLINE: its stdout is its output, and asking a server here would
// make `kb version` hang or fail when no server is up.
func runVersion(args []string, out io.Writer) (handled bool) {
	if len(args) >= 1 && args[0] == "version" {
		fmt.Fprintln(out, version.String())
		return true
	}
	return false
}

// versionCheckTimeout bounds the proxy's version check. It runs beside the
// proxy loop, never in front of it, so it cannot delay the MCP handshake; the
// bound only keeps a hung server from leaving the goroutine around.
const versionCheckTimeout = 2 * time.Second

// warnVersionSkew asks the server for its build version (GET
// /api/v1/version) and, when it differs from this kb's, writes ONE line to w
// naming both. A kb and a server from different builds disagree on tool
// shapes in ways neither can report (the first mission ran a stale kb against
// a newer server), so the mismatch is worth one line; it is never worth
// failing the command over.
//
// Anything that stops the comparison — no answer, an error status, an
// undecodable body, an empty version — writes nothing: a warning that rests
// on a signal we do not have would be noise. c must carry its own timeout.
// Only the long-lived proxy calls this, and only with stderr: stdout is the
// MCP stream.
func warnVersionSkew(w io.Writer, c *http.Client, base, serverName, local string) {
	resp, err := c.Get(base + "/api/v1/version") //nolint:noctx // c carries the timeout
	if err != nil {
		log.Debug().Err(err).Msg("version check: no answer")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Debug().Int("status", resp.StatusCode).Msg("version check: not OK")
		return
	}
	var body struct {
		Full string `json:"full"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Full == "" {
		log.Debug().Err(err).Msg("version check: no version in the answer")
		return
	}
	if body.Full == local {
		return
	}
	log.Warn().Str("kb", local).Str("server", body.Full).Msg("kb and server versions differ")
	fmt.Fprintf(w, "kb: version %s differs from the server's %s at %s; update whichever is older\n",
		local, body.Full, serverName)
}
