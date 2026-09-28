package claude

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"knomit/internal/repos"
	"knomit/tools/bridge/knomitapi"
)

// skipMultipleKnomitServers is the skip reason for a project configuring more
// than one knomit server. Named because both helpers.go and the session-start
// hook must agree on it: the hook special-cases this reason to tell the user,
// since unlike every other skip it is a misconfiguration that never resolves
// on its own.
const skipMultipleKnomitServers = "multiple_knomit_servers"

// skipUnbound is the skip reason for a knomit entry with neither --repo nor
// --lens. The session-start hook answers it with knomitapi.UnboundNote.
const skipUnbound = knomitapi.SkipUnbound

// isKnomitCommand reports whether an .mcp.json entry's COMMAND identifies the
// knomit bridge. The implementation is shared with the Antigravity host — see
// knomitapi.IsKnomitCommand — so the two cannot drift.
func isKnomitCommand(command string) bool {
	return knomitapi.IsKnomitCommand(command)
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
	return knomitapi.IsKnomitKey(key)
}

// isKnomitServer reports whether an .mcp.json entry is a knomit bridge by
// either signal. Callers that must distinguish proof from guess use the two
// predicates directly.
func isKnomitServer(key, command string) bool {
	return isKnomitCommand(command) || isKnomitKey(key)
}

// mcpEntry is one knomit server entry of an .mcp.json: its key and its args.
type mcpEntry struct {
	key  string
	args []string
}

// knomitEntries returns every knomit server entry in an .mcp.json document,
// and found=false when the document does not index or has none.
//
// It reads the file with indexJSON — the lenient, position-keeping parser the
// .mcp.json MERGE uses — so the hooks, init's keep guard and the merge can
// never disagree about which knomit entries exist. A strict struct decode
// failed on any unrelated server with a non-string arg or an array command,
// and then saw no knomit entry at all: the hooks fell back to the directory
// basename, and a flagless init unbound the same-key entry it should have kept.
// A value this parser cannot read as a string is taken as its raw JSON text.
//
// Select by COMMAND, not by key. The key used to be the constant "knomit",
// but `claude init` now derives it from the scope so that two knomit servers
// can coexist in one project — keying off it would silently unbind every
// hook the moment a project scaffolds as anything but a knomit-named repo,
// which is precisely the wrong-repo hazard the lens rules exist to avoid.
// The command is what actually identifies a knomit server.
//
// Command matches are proof; key matches are a guess (isKnomitKey). A guess
// must never dilute proof: a project running one real bridge alongside an
// unrelated server that merely borrowed the `knomit-` namespace would
// otherwise count two matches and disable every hook, telling the user to
// remove a knomit entry they do not have. So key matches are considered only
// when nothing matched on command at all — which is exactly the legacy /
// wrapper-script case the key fallback exists for.
func knomitEntries(data []byte) (entries []mcpEntry, found bool) {
	root, err := indexJSON(data)
	if err != nil {
		return nil, false
	}
	servers := root.child("mcpServers")
	if !servers.object() {
		return nil, false
	}
	var byCommand, byKey []mcpEntry
	for _, key := range servers.order {
		command, args := entryFields(data, servers.child(key))
		e := mcpEntry{key: key, args: args}
		switch {
		case isKnomitCommand(command):
			byCommand = append(byCommand, e)
		case isKnomitKey(key):
			byKey = append(byKey, e)
		}
	}
	if len(byCommand) > 0 {
		return byCommand, true
	}
	return byKey, len(byKey) > 0
}

// entryFields reads one server entry's command and args without failing on
// shapes a strict decode rejects: a command that is not a string reads as "",
// and an arg that is not a string is kept as its raw JSON text.
func entryFields(data []byte, srv *jsonNode) (command string, args []string) {
	if !srv.object() {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data[srv.span.open:srv.span.close+1], &fields) != nil {
		return "", nil
	}
	_ = json.Unmarshal(fields["command"], &command)
	var raw []json.RawMessage
	if json.Unmarshal(fields["args"], &raw) != nil {
		return command, nil
	}
	args = make([]string, len(raw))
	for i, r := range raw {
		if json.Unmarshal(r, &args[i]) != nil {
			args[i] = string(r)
		}
	}
	return command, args
}

// mcpBinding resolves the knomit MCP server config in .mcp.json under
// projectDir to the scope the hooks bind to, or a skip reason. The rule is
// knomitapi.SingleScope, shared with the Antigravity host, so the two cannot
// drift:
//
//   - repo != "": repo mode — the entry's --repo, or the projectDir basename
//     when the project has NO knomit entry at all (no .mcp.json, or no kb
//     server in it). That fallback is the only one; it is deferred, not
//     endorsed (#341).
//   - lens != "": lens mode. The caller resolves the lens's write repo via the
//     API and skips cleanly on failure.
//   - skip == skipUnbound: the entry carries neither flag, so the server starts
//     unbound. Read from the ARGS alone — the key may carry the directory name
//     (knomitapi.UnboundServerKey) and names nothing.
//   - skip == skipMultipleKnomitServers: entries resolving to DIFFERENT targets.
//     Entries naming the same scope are not ambiguous: `.mcp.json` is
//     merge-required, so re-running init can leave the old entry beside the
//     new one, both naming the same repo.
//   - any other skip: a BROKEN entry (a flag with a missing or empty value, or
//     an invalid name). It never falls back to the basename.
func mcpBinding(projectDir string) (repo, lens, skip string) {
	data, err := os.ReadFile(filepath.Join(projectDir, ".mcp.json"))
	if err != nil {
		return filepath.Base(projectDir), "", ""
	}
	entries, found := knomitEntries(data)
	if !found {
		return filepath.Base(projectDir), "", ""
	}
	args := make([][]string, len(entries))
	for i, e := range entries {
		args[i] = e.args
	}
	s, skip := knomitapi.SingleScope(args, repos.IsValidName)
	if skip == knomitapi.SkipAmbiguous {
		skip = skipMultipleKnomitServers
	}
	return s.Repo, s.Lens, skip
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
// reason.
//
// Lens mode: resolves the lens's WRITE repo via GET /api/v1/lenses/{name}. On
// any error (server down, 404, decode) it returns ("", "lens_unresolved") so
// the hook skips cleanly — a lens-configured session NEVER falls back to the
// basename, which could name an unrelated repo and run the hook against the
// wrong data.
//
// Every skip mcpBinding reports is passed through: unbound, more than one
// scope, or a broken entry.
//
// Scope note: hook reads are deliberately write-repo-scoped. Until lens
// *browsing* REST exists (backlog A.1), the write repo is where the session's
// facts land, so session-start / post-edit context stays accurate for the
// write side.
func resolveWriteRepo(projectDir string) (repo, skipReason string) {
	r, lens, skip := mcpBinding(projectDir)
	if skip != "" {
		return "", skip
	}
	if r != "" {
		return r, ""
	}
	w := knomitapi.LensWriteRepo(lens)
	if w == "" {
		return "", "lens_unresolved"
	}
	return w, ""
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
// that back is correct by construction: rewiring `kb claude hook post-edit`
// from PostToolUse to PostToolBatch (or Stop, or any other
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
