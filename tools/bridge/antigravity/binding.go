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
// the hook's own output — can say which one happened.
const (
	skipNoBinding        = "no_binding"
	skipAmbiguousBinding = "ambiguous_binding"
	skipLensUnusable     = "lens_unusable"
	skipInvalidScope     = "invalid_scope"
	skipLensUnresolved   = "lens_unresolved"
)

// target is one config entry's resolved knomit scope.
type target struct {
	repo     string
	lens     string
	lensMode bool
}

// pluginBinding reads mcp_config.json in pluginDir and returns the knomit scope
// it configures, or a skip reason.
//
// It deliberately does NOT stop at the first match. `init` writes exactly one
// entry, but this file lives in the user's workspace and is writable by them, a
// monorepo scaffolder, or a second tool — and returning whichever entry a Go map
// happened to yield first made the binding run-dependent, so a user could see
// another project's facts intermittently with no reproducible trigger. Every
// match is collected and compared; disagreement is a skip, never a coin flip.
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

	var byCommand, byKey []target
	for _, k := range keys {
		srv := cfg.MCPServers[k]
		r, l, mode := knomitapi.ClassifyArgs(srv.Args)
		t := target{repo: r, lens: l, lensMode: mode}
		switch {
		case knomitapi.IsKnomitCommand(srv.Command):
			byCommand = append(byCommand, t)
		case knomitapi.IsKnomitKey(k):
			byKey = append(byKey, t)
		}
	}
	matches := byCommand
	if len(matches) == 0 {
		matches = byKey
	}
	if len(matches) == 0 {
		return "", "", skipNoBinding
	}

	// Every match must agree. Two entries naming the same scope are fine — that
	// is what a duplicated-but-consistent edit looks like — but two different
	// scopes have no principled answer, so skip rather than pick one.
	first := matches[0]
	for _, t := range matches[1:] {
		if t != first {
			return "", "", skipAmbiguousBinding
		}
	}

	// A lens-configured entry whose name is missing or empty is unusable, and
	// it must NOT degrade into a repo scope. Checking lensMode rather than
	// (lens != "") is what stops a stray or sibling --repo from winning here.
	if first.lensMode {
		if first.lens == "" {
			return "", "", skipLensUnusable
		}
		if !repos.IsValidName(first.lens) {
			return "", "", skipInvalidScope
		}
		return "", first.lens, ""
	}
	if first.repo == "" {
		return "", "", skipNoBinding
	}
	// Re-validate on READ, not just on write. `init` validates before writing,
	// but this file is hand-editable afterwards, and the value is interpolated
	// into an API path — a name containing `..`, `?` or `#` would otherwise
	// reach the server as a different resource.
	if !repos.IsValidName(first.repo) {
		return "", "", skipInvalidScope
	}
	return first.repo, "", ""
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
