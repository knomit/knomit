package antigravity

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/tools/bridge/knomitapi"
)

// markerMaxAge bounds how long a per-conversation marker lingers. Pruning is
// opportunistic (on the emitting path only) so the common no-op path never
// walks the directory.
const markerMaxAge = 30 * 24 * time.Hour

// Skip reasons specific to this hook. The binding ones live in binding.go.
const (
	skipBadInput           = "bad_input"
	skipNoInvocationNum    = "missing_invocation_num"
	skipNotFirstInvocation = "not_first_invocation"
	skipUnusableConvID     = "unusable_conversation_id"
	skipAlreadyGreeted     = "already_greeted"
	skipNoPluginDir        = "no_plugin_dir"
	skipNoAgentBranch      = "no_agent_branch"
)

// pluginDirName is the last path element of the directory this hook's
// hooks.json lives in. Used only as a sanity check on the working directory.
const pluginDirName = "knomit"

type preInvocationInput struct {
	// InvocationNum is a POINTER on purpose. Antigravity's hook payload is a
	// beta API, and a plain int would decode an absent, renamed or restructured
	// field to Go's zero value 0 — which this hook reads as "first invocation
	// of the conversation". Combined with an absent conversationId (no marker
	// possible), that failed OPEN: the entire corpus block was prepended to
	// EVERY model call, forever. A nil pointer is distinguishable and skips.
	InvocationNum  *int     `json:"invocationNum"`
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
}

type injectStep struct {
	EphemeralMessage string `json:"ephemeralMessage"`
}

type preInvocationOutput struct {
	InjectSteps []injectStep `json:"injectSteps"`
}

// configSkips are the skip reasons a user can actually act on, and which never
// resolve on their own. Every other skip is transient (server down) or benign
// (no facts yet) and stays quiet. This mirrors the Claude host, which singles
// out its own never-self-resolving misconfiguration because "the user cannot
// see the log field — so say it out loud".
var configSkips = map[string]string{
	skipNoBinding: "knomit found no usable knomit server in this plugin's mcp_config.json, " +
		"so it cannot tell which repo to read. Re-run `knomit-bridge antigravity init` in this project.",
	skipAmbiguousBinding: "knomit is DISABLED here: this plugin's mcp_config.json names more than one " +
		"knomit scope, so there is no single repo to bind to. Leave exactly one entry to re-enable it.",
	skipLensUnusable: "knomit is DISABLED here: this plugin's mcp_config.json has a --lens flag with no " +
		"value. Give the lens a name, or switch the entry to --repo <name>.",
	skipInvalidScope: "knomit is DISABLED here: the repo or lens name in this plugin's mcp_config.json is " +
		"not a valid knomit name (lowercase letters, digits, hyphens, underscores).",
	skipNoPluginDir: "knomit could not locate its plugin directory from this hook's working directory, " +
		"so it cannot read its own configuration. This usually means the plugin was installed globally " +
		"with `agy plugin install` rather than scaffolded into the project.",
}

// hookPreInvocation injects the knomit corpus context once per conversation.
//
// Antigravity has no session-start event, so this runs on PreInvocation, which
// fires before EVERY model call. It emits only when the platform reports the
// first invocation AND no marker exists for this conversation. Both guards must
// be affirmatively satisfied — a missing field is a skip, never an implied yes.
//
// Every path writes a valid JSON object and returns nil. A hook must never
// break the agent loop, and that includes never returning an error for a failed
// write: returning one makes main exit non-zero, which agy reads as a failed
// hook.
func hookPreInvocation(r io.Reader, w io.Writer) error {
	var (
		emitted    bool
		stats      knomitapi.Stats
		skipReason string
	)
	defer func() {
		ev := log.Info().Str("event", "pre-invocation").Bool("emitted", emitted)
		if skipReason != "" {
			ev.Str("skip_reason", skipReason)
		}
		if emitted {
			ev.Int("globals", stats.Globals).
				Int("invariants_fallback", stats.InvariantsFallback).
				Int("recent", stats.Recent)
		}
		ev.Msg("hook result")
	}()

	var in preInvocationInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		skipReason = skipBadInput
		return emitEmpty(w)
	}
	if in.InvocationNum == nil {
		// The field the whole once-per-conversation guard rests on is absent.
		// Fail closed: staying silent costs one greeting, failing open costs
		// the corpus block on every model call for the life of the session.
		skipReason = skipNoInvocationNum
		return emitEmpty(w)
	}
	if *in.InvocationNum != 0 {
		skipReason = skipNotFirstInvocation
		return emitEmpty(w)
	}

	marker := markerPath(in.ConversationID)
	if marker == "" {
		// No usable conversation id means the marker guard cannot function.
		// Fail closed for the same reason as above.
		skipReason = skipUnusableConvID
		return emitEmpty(w)
	}
	if _, err := os.Stat(marker); err == nil {
		skipReason = skipAlreadyGreeted
		return emitEmpty(w)
	}

	pluginDir, ok := locatePluginDir(in.WorkspacePaths)
	if !ok {
		skipReason = skipNoPluginDir
		return emitNotice(w, configSkips[skipNoPluginDir])
	}
	repo, skip := resolveWriteRepo(pluginDir)
	if skip != "" {
		skipReason = skip
		if msg, visible := configSkips[skip]; visible {
			return emitNotice(w, msg)
		}
		return emitEmpty(w)
	}
	branch := knomitapi.AgentBranch(repo)
	if branch == "" {
		skipReason = skipNoAgentBranch
		return emitEmpty(w)
	}

	text, st := knomitapi.SessionContext(repo, branch)
	stats = st
	if text == "" {
		skipReason = st.SkipReason
		return emitEmpty(w)
	}

	// Encode FIRST, then record the marker. The reverse order meant a failed
	// write left the conversation permanently marked as greeted while the
	// context was never delivered — the opposite of writeMarker's own contract
	// that a duplicate greeting beats a lost one.
	out := preInvocationOutput{InjectSteps: []injectStep{{EphemeralMessage: text}}}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Warn().Err(err).Msg("pre-invocation: encode failed; not marking conversation greeted")
		skipReason = "encode_failed"
		return nil
	}
	writeMarker(marker)
	emitted = true
	return nil
}

// emitEmpty writes the no-op envelope. Antigravity expects a JSON object from
// every hook; anything else is treated as a failure.
func emitEmpty(w io.Writer) error {
	if _, err := io.WriteString(w, "{}\n"); err != nil {
		log.Warn().Err(err).Msg("pre-invocation: writing empty envelope failed")
	}
	return nil
}

// emitNotice injects a one-off message about a misconfiguration the user can
// fix. Used only for states that never resolve on their own; a memory system
// whose hook goes silently dark is its worst failure mode.
func emitNotice(w io.Writer, msg string) error {
	out := preInvocationOutput{InjectSteps: []injectStep{{EphemeralMessage: msg}}}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Warn().Err(err).Msg("pre-invocation: encoding notice failed")
	}
	return nil
}

// locatePluginDir returns the directory holding this hook's mcp_config.json.
//
// Antigravity runs a hook with its working directory set to the directory
// containing the hooks.json that registered it, which for a project-scaffolded
// plugin is <workspace>/.agents/plugins/knomit. That is the primary source. It
// is checked rather than trusted, and workspacePaths is used as a fallback, so
// that a platform change to hook cwd degrades to a named skip instead of a
// silent one.
func locatePluginDir(workspacePaths []string) (string, bool) {
	if cwd, err := os.Getwd(); err == nil {
		if filepath.Base(cwd) == pluginDirName {
			return cwd, true
		}
		// cwd is not our plugin dir; it may still be the workspace root.
		if candidate := filepath.Join(cwd, PluginDir); isPluginDir(candidate) {
			return candidate, true
		}
	}
	for _, ws := range workspacePaths {
		if ws == "" {
			continue
		}
		if candidate := filepath.Join(ws, PluginDir); isPluginDir(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isPluginDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "mcp_config.json"))
	return err == nil
}

// markerPath returns the marker file for a conversation, or "" when the id is
// unusable. The id is opaque agent-supplied text, so it is restricted to a
// path-safe alphabet rather than trusted: a value containing separators or
// ".." could otherwise steer the write out of the cache directory.
func markerPath(conversationID string) string {
	if conversationID == "" || !isPathSafe(conversationID) {
		return ""
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cache, "knomit", "agy-sessions", conversationID)
}

// windowsReserved are device names that resolve to an OS device rather than a
// file on Windows, so a marker written to one would never persist and the
// conversation would be greeted again on every invocation.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func isPathSafe(s string) bool {
	if len(s) > 128 {
		return false
	}
	if windowsReserved[strings.ToLower(s)] {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '-' || r == '_':
			return false
		}
		return true
	}) < 0
}

// writeMarker records that this conversation has been greeted, and prunes stale
// markers. Best-effort throughout: failing to write one means at worst a
// duplicate greeting, which is far better than failing the hook.
func writeMarker(path string) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn().Err(err).Str("dir", dir).Msg("pre-invocation: cannot create marker dir")
		return
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("pre-invocation: cannot write marker")
		return
	}
	pruneMarkers(dir)
}

func pruneMarkers(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-markerMaxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
