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

// encodeBranch URL-encodes a branch name for inclusion in a knomit API path.
// Branches with slashes (e.g. "machine/host") are substituted "/" → ":" per
// the project convention; the server's branch-route handler does the reverse
// substitution. See kb/conventions/web/branch-slash-colon-substitution.
func encodeBranch(branch string) string {
	return strings.ReplaceAll(branch, "/", ":")
}

// knomitBaseURL returns the knomit HTTP base URL.
// Set KNOMIT_BASE_URL for non-default ports; otherwise the default works
// for a standard local install.
func knomitBaseURL() string {
	if u := os.Getenv("KNOMIT_BASE_URL"); u != "" {
		return u
	}
	return "http://localhost:19278"
}

// skipMultipleKnomitServers is the skip reason for a project configuring more
// than one knomit server. Named because both helpers.go and the session-start
// hook must agree on it: the hook special-cases this reason to tell the user,
// since unlike every other skip it is a misconfiguration that never resolves
// on its own.
const skipMultipleKnomitServers = "multiple_knomit_servers"

// isKnomitCommand reports whether an .mcp.json entry's COMMAND identifies the
// knomit bridge. This is the certain signal: it names the actual process and it
// survives the key being derived per scope. `.exe` is trimmed because the
// Makefile builds a GOOS=windows target, where the command is knomit-bridge.exe
// and a bare basename comparison would miss every Windows install.
func isKnomitCommand(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "knomit-bridge"
}

// isKnomitKey reports whether an .mcp.json KEY looks like a knomit server. This
// is a guess, not proof: a wrapper script, a renamed symlink, a versioned binary
// or `go run` all leave a command isKnomitCommand cannot recognise, and those
// configs resolved fine when the lookup was `cfg.MCPServers["knomit"]`. Failing
// to match them does not fail safe — it falls through to the basename fallback,
// which is the wrong-repo hazard this file's contract forbids.
//
// Because it is a guess it also fires on servers that merely borrowed the
// namespace (an unrelated `knomit-notes` MCP server). mcpBinding therefore
// treats key matches as a strictly lower tier than command matches: see the
// selection there.
func isKnomitKey(key string) bool {
	return key == "knomit" || strings.HasPrefix(key, "knomit-")
}

// isKnomitServer reports whether an .mcp.json entry is a knomit bridge by
// either signal. Callers that must distinguish proof from guess use the two
// predicates directly.
func isKnomitServer(key, command string) bool {
	return isKnomitCommand(command) || isKnomitKey(key)
}

// mcpBinding classifies the knomit MCP server config in .mcp.json under
// projectDir into either lens mode or repo mode. It is pure (no I/O beyond the
// single file read) so the classification is unit-testable in isolation.
//
// Returns (repo, lens, ambiguous):
//   - ambiguous: the project configures MORE THAN ONE knomit server RESOLVING
//     TO DIFFERENT TARGETS. There is no principled answer to which repo the
//     hooks should bind to, so repo and lens are both "" and the caller must
//     skip rather than pick one. Entries that resolve to the same target are
//     not ambiguous — see the selection below.
//   - lens != "": lens mode — the file configures --lens <name>. repo is "".
//     A lens-configured file NEVER falls back to the basename; the caller must
//     resolve the write repo via the API and skip cleanly on failure.
//   - lens == "", repo != "": repo mode — the --repo <name> arg, or the
//     projectDir basename fallback when there is no readable/parseable knomit
//     config and no flag to read.
//
// Precedence when both flags appear (which `claude init` forbids, but a
// hand-edited .mcp.json could contain): --lens wins, regardless of argument
// order. A stray --repo must not demote a lens-configured session to a raw
// repo scope — lens mode resolves via the API and fails safe (skips) rather
// than risk reading the wrong repo.
func mcpBinding(projectDir string) (repo, lens string, ambiguous bool) {
	base := filepath.Base(projectDir)
	data, err := os.ReadFile(filepath.Join(projectDir, ".mcp.json"))
	if err != nil {
		return base, "", false
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return base, "", false
	}

	// Select by COMMAND, not by key. The key used to be the constant "knomit",
	// but `claude init` now derives it from the scope so that two knomit servers
	// can coexist in one project — keying off it would silently unbind every
	// hook the moment a project scaffolds as anything but a knomit-named repo,
	// which is precisely the wrong-repo hazard the lens rules below exist to
	// avoid. The command is what actually identifies a knomit server.
	//
	// Command matches are proof; key matches are a guess (isKnomitKey). A guess
	// must never dilute proof: a project running one real bridge alongside an
	// unrelated server that merely borrowed the `knomit-` namespace would
	// otherwise count two matches and disable every hook, telling the user to
	// remove a knomit entry they do not have. So key matches are considered only
	// when nothing matched on command at all — which is exactly the legacy /
	// wrapper-script case the key fallback exists for.
	var byCommand, byKey []string
	for key, srv := range cfg.MCPServers {
		switch {
		case isKnomitCommand(srv.Command):
			byCommand = append(byCommand, key)
		case isKnomitKey(key):
			byKey = append(byKey, key)
		}
	}
	matches := byCommand
	if len(matches) == 0 {
		matches = byKey
	}
	if len(matches) == 0 {
		return base, "", false
	}
	repo, lens = classifyArgs(cfg.MCPServers[matches[0]].Args, base)
	// Two or more knomit servers that name DIFFERENT targets: there is no
	// principled answer to "which repo do the hooks bind to?", and guessing runs
	// post-edit against possibly the wrong repo. Fail safe — the caller skips
	// and says why.
	//
	// Same-target duplicates are not that case, and they are the likely one:
	// `.mcp.json` is merge-required, so re-running init drops a companion and the
	// obvious merge leaves the pre-existing entry beside the freshly derived one,
	// both pointing at the same repo. Disabling the hooks over a config with a
	// single unambiguous answer would be a fail-safe that fires on nothing.
	// Map iteration order is random, so the comparison must be order-independent
	// — it is: we return only when every match agrees.
	for _, key := range matches[1:] {
		r, l := classifyArgs(cfg.MCPServers[key].Args, base)
		if r != repo || l != lens {
			return "", "", true
		}
	}
	return repo, lens, false
}

// classifyArgs maps one server entry's args to its (repo, lens) target, with
// base as the repo-mode fallback. Split out of mcpBinding so the multi-server
// path can compare targets without duplicating the precedence rules.
func classifyArgs(args []string, base string) (repo, lens string) {
	var repoArg string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--lens" || a == "-lens":
			// A --lens token means lens mode even when the value is missing
			// (a hand-mangled config): lens wins and we NEVER fall back to the
			// basename. A degenerate --lens with no value yields an empty lens
			// name, which resolveWriteRepo/lensWriteRepo turn into a clean skip
			// rather than the wrong-repo hazard a basename fallback would
			// reintroduce.
			if i+1 < len(args) {
				return "", args[i+1]
			}
			return "", "" // lens flag with no value: lens mode, empty name
		case strings.HasPrefix(a, "--lens=") || strings.HasPrefix(a, "-lens="):
			// Go-flag combined form. Lens wins immediately, exactly like the
			// token arm; an empty value after '=' yields an empty lens name
			// (clean skip downstream), never a basename fallback.
			_, v, _ := strings.Cut(a, "=")
			return "", v
		case a == "--repo" || a == "-repo":
			if repoArg == "" && i+1 < len(args) {
				repoArg = args[i+1]
			}
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "-repo="):
			if repoArg == "" {
				_, v, _ := strings.Cut(a, "=")
				repoArg = v
			}
		}
	}
	if repoArg != "" {
		return repoArg, ""
	}
	return base, ""
}

// repoFromMCP returns the repo-mode target for projectDir (the --repo arg or
// the basename fallback). It is a thin wrapper over mcpBinding retained for the
// repo-mode call path and its regression tests; a lens-configured file yields
// "" here, so lens-aware callers must use resolveWriteRepo instead.
func repoFromMCP(projectDir string) string {
	repo, _, _ := mcpBinding(projectDir)
	return repo
}

// resolveWriteRepo maps a project directory to the knomit repo whose
// agent_branch and facts the hooks should read.
//
// Repo mode: returns the configured repo (or basename) with an empty skip
// reason — behavior is byte-identical to the pre-lens repoFromMCP path.
//
// Lens mode: resolves the lens's WRITE repo via GET /api/v1/lenses/{name}. On
// any error (server down, 404, decode) it returns ("", "lens_unresolved") so
// the hook skips cleanly — a lens-configured session NEVER falls back to the
// basename, which could name an unrelated repo and run the hook against the
// wrong data.
//
// Scope note: hook reads are deliberately write-repo-scoped. Until lens
// *browsing* REST exists (backlog A.1), the write repo is where the session's
// facts land, so session-start / post-edit context stays accurate for the
// write side.
func resolveWriteRepo(projectDir string) (repo, skipReason string) {
	r, lens, ambiguous := mcpBinding(projectDir)
	if ambiguous {
		// More than one knomit server in this project. Binding to an arbitrary
		// one would run post-edit against possibly the wrong repo, so skip and
		// say why rather than go silently dark.
		return "", skipMultipleKnomitServers
	}
	if r != "" {
		return r, "" // repo mode: configured --repo or basename fallback
	}
	// Lens mode: mcpBinding leaves repo empty. lens may itself be empty for a
	// hand-mangled --lens with no value; that resolves to "" and skips cleanly
	// below rather than falling back to the basename.
	w := lensWriteRepo(lens)
	if w == "" {
		return "", "lens_unresolved"
	}
	return w, ""
}

// lensWriteRepo queries knomit for a lens's write repo. The lens resource names
// each member as a {uid, name} pair: uid is the registry key membership is stored
// under, name is the server-resolved display name. This returns the NAME, because
// the sole caller feeds it to agentBranch, and /api/v1/repos/{repo} is
// name-addressed — handing it a uid would 404 on every lens-mode hook.
//
// Returns "" on any error — mirroring the graceful degradation of agentBranch /
// fetchFacts — with every failure path logged at Warn so a dead server is visible
// in the bridge log rather than silent.
func lensWriteRepo(name string) string {
	u := fmt.Sprintf("%s/api/v1/lenses/%s", knomitBaseURL(), name)
	resp, err := hookHTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("lensWriteRepo: GET failed")
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("lensWriteRepo: non-200")
		return ""
	}
	var body struct {
		Write struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"write"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("lensWriteRepo: decode failed")
		return ""
	}
	return body.Write.Name
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

// emitAdditionalContext writes a JSON object to w that injects ctx as a
// system reminder via CC's hookSpecificOutput.additionalContext mechanism.
// Returns nil if ctx is empty (caller can short-circuit before any output).
//
// event MUST be the CC hook event this hook was dispatched for — NOT the
// bridge's own subcommand name. CC compares it against the event it dispatched
// and throws "Hook returned incorrect event name: expected 'X' but got 'Y'",
// discarding the entire payload, so getting it wrong silently costs the nudge,
// not just the field. Callers must pass wiredEvent(in.HookEventName, …) rather
// than a hardcoded literal: re-wiring a hook to a different event in
// settings.json would otherwise reintroduce exactly that mismatch.
//
// Naming the wired event is necessary but NOT sufficient: hookSpecificOutput is
// a discriminated union, and only twelve events carry an additionalContext
// variant — PreToolUse, PostToolUse, PostToolBatch, PostToolUseFailure,
// UserPromptSubmit, UserPromptExpansion, SessionStart, Setup, SubagentStart,
// Stop, SubagentStop, Notification. (Read off CC 2.1.226's actual output
// schema; the shorter list in CC's own "Expected schema:" error hint is a lossy
// subset — do not trust it.) Every other event — PreCompact, SessionEnd,
// PermissionRequest, … — fails validation no matter what is passed here; those
// hooks must emit plain text on stdout instead. Note that plain stdout is NOT
// uniformly injected into the model's context either: each event routes it
// somewhere event-specific, so check before relying on it — see hookPreCompact
// (routed into the summarizer's prompt) and hookSessionStart.
func emitAdditionalContext(w io.Writer, event, ctx string) error {
	if ctx == "" {
		return nil
	}
	payload := struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}{}
	payload.HookSpecificOutput.HookEventName = event
	payload.HookSpecificOutput.AdditionalContext = ctx
	return json.NewEncoder(w).Encode(payload)
}

// wiredEvent picks the event name to echo back in hookSpecificOutput. CC puts
// hook_event_name on the stdin payload of every hook it dispatches, so echoing
// that back is correct by construction: rewiring `knomit-bridge claude hook
// post-edit` from PostToolUse to PostToolBatch (or Stop, or any other
// additionalContext-carrying event) in settings.json keeps working, where a
// hardcoded literal would trip CC's expected-vs-got check and drop the nudge.
//
// fallback covers a payload that omitted the field — malformed input, not
// something CC produces — and should be the event the hook is wired to in
// settings.json.tmpl.
func wiredEvent(fromInput, fallback string) string {
	if fromInput == "" {
		return fallback
	}
	return fromInput
}
