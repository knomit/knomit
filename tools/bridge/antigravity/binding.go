package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"knomit/internal/repos"
	"knomit/tools/bridge/knomitapi"
)

// Skip reasons returned by resolveWriteRepo. Each names a distinct
// misconfiguration so the bridge log — and, for the ones a user can act on,
// the hook's own output — can say which one happened. The binding reasons are
// knomitapi's, shared with the Claude Code host.
const (
	skipNoBinding        = knomitapi.SkipNoBinding
	skipAmbiguousBinding = knomitapi.SkipAmbiguous
	skipLensUnusable     = knomitapi.SkipLensUnusable
	skipInvalidScope     = knomitapi.SkipInvalidScope
	skipLensUnresolved   = "lens_unresolved"
	// skipUnbound is not a misconfiguration: the entry starts knomit unbound
	// and the agent binds with knomit_bind. The hook answers it with
	// knomitapi.UnboundNote.
	skipUnbound = knomitapi.SkipUnbound
)

// pluginBinding reads mcp_config.json in pluginDir and returns the knomit scope
// it configures, or a skip reason.
//
// It deliberately does NOT stop at the first match. `init` writes exactly one
// entry, but this file lives in the user's workspace and is writable by them, a
// monorepo scaffolder, or a second tool — and returning whichever entry a Go map
// happened to yield first made the binding run-dependent, so a user could see
// another project's facts intermittently with no reproducible trigger. Every
// match is handed to knomitapi.SingleScope, which skips on disagreement rather
// than flipping a coin, and is the same rule the Claude Code host uses.
//
// Matching is two-tiered, mirroring the Claude host: COMMAND matches are proof,
// KEY matches are a guess consulted only when nothing matched on command. The
// key tier is what keeps a wrapper script, a dev build, a renamed symlink, a
// versioned binary or `go run` working — dropping it made every such install
// silently dark.
//
// There is NO basename fallback. A missing or unparseable file means the
// scaffold is broken; guessing a repo from the directory name could point the
// hook at an unrelated knowledge base.
func pluginBinding(pluginDir string) (repo, lens string, skip string) {
	data, err := os.ReadFile(filepath.Join(pluginDir, "mcp_config.json"))
	if err != nil {
		return "", "", skipNoBinding
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "", skipNoBinding
	}

	// Sort the keys so the two tiers are built deterministically; map order
	// must not influence anything this function returns.
	keys := make([]string, 0, len(cfg.MCPServers))
	for k := range cfg.MCPServers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var byCommand, byKey [][]string
	for _, k := range keys {
		srv := cfg.MCPServers[k]
		switch {
		case knomitapi.IsKnomitCommand(srv.Command):
			byCommand = append(byCommand, srv.Args)
		case knomitapi.IsKnomitKey(k):
			byKey = append(byKey, srv.Args)
		}
	}
	matches := byCommand
	if len(matches) == 0 {
		matches = byKey
	}
	s, skip := knomitapi.SingleScope(matches, repos.IsValidName)
	return s.Repo, s.Lens, skip
}

// resolveWriteRepo maps the plugin directory to the knomit repo whose
// agent_branch and facts the hook should read.
//
// Repo mode returns the configured repo. Lens mode resolves the lens's WRITE
// repo via the API and returns a skip on any failure, so the hook stays quiet
// rather than reading an unrelated repo.
func resolveWriteRepo(pluginDir string) (repo, skipReason string) {
	r, lens, skip := pluginBinding(pluginDir)
	if skip != "" {
		return "", skip
	}
	if r != "" {
		return r, ""
	}
	w := knomitapi.LensWriteRepo(lens)
	if w == "" {
		return "", skipLensUnresolved
	}
	// The server names the write repo; validate it before it becomes a URL path
	// segment, on the same principle as the configured names above.
	if !repos.IsValidName(w) {
		return "", skipInvalidScope
	}
	return w, ""
}
