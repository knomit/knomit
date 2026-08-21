package knomitapi

import (
	"path/filepath"
	"strings"
)

// IsKnomitCommand reports whether an MCP-config entry's COMMAND identifies the
// knomit bridge. This is the certain signal: it names the actual process and it
// survives the config key being derived per scope. `.exe` is trimmed because the
// Makefile builds a GOOS=windows target, where the command is knomit-bridge.exe
// and a bare basename comparison would miss every Windows install.
func IsKnomitCommand(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "knomit-bridge"
}

// IsKnomitKey reports whether an MCP-config KEY looks like a knomit server.
// This is a guess, not proof: a wrapper script, a renamed symlink, a versioned
// binary or `go run` all leave a command IsKnomitCommand cannot recognise, and
// those configs are legitimate. Callers must treat key matches as a strictly
// LOWER tier than command matches, consulted only when nothing matched on
// command — otherwise a server that merely borrowed the `knomit-` namespace
// would dilute a real match.
func IsKnomitKey(key string) bool {
	return key == "knomit" || strings.HasPrefix(key, "knomit-")
}

// ClassifyArgs maps one MCP server entry's args to the knomit scope it targets.
//
// lensMode is the load-bearing third value: it is true whenever a `--lens`
// token appears AT ALL, including a degenerate one whose value is missing or
// empty. Callers must treat lensMode with an empty lens as "this entry is
// lens-configured but unusable" — a clean skip — and must NEVER fall through to
// a repo scope, whether that repo comes from a sibling config entry or from a
// directory-basename fallback. Collapsing that case into "no target" is what
// lets a stray --repo demote a lens-configured plugin to a raw repo scope, and
// reading the wrong knowledge base is the exact hazard the lens rules exist to
// prevent.
//
// Precedence when both flags appear (which every init path forbids, but a
// hand-edited file can contain): --lens wins regardless of argument order.
//
// Both `-flag value` and `--flag=value` forms are accepted, since these files
// are hand-editable.
func ClassifyArgs(args []string) (repo, lens string, lensMode bool) {
	var repoArg string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--lens" || a == "-lens":
			if i+1 < len(args) {
				return "", args[i+1], true
			}
			return "", "", true // lens token, no value: lens mode, unusable
		case strings.HasPrefix(a, "--lens=") || strings.HasPrefix(a, "-lens="):
			_, v, _ := strings.Cut(a, "=")
			return "", v, true
		case a == "--repo" || a == "-repo":
			// Only consume a value that is not itself a flag: `--repo --repo x`
			// must not bind to a repo literally named "--repo".
			if repoArg == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				repoArg = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "-repo="):
			if repoArg == "" {
				_, v, _ := strings.Cut(a, "=")
				repoArg = v
			}
		}
	}
	return repoArg, "", false
}
