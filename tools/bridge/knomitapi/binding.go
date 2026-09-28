package knomitapi

import (
	"path/filepath"
	"strings"
)

// IsKnomitCommand reports whether an MCP-config entry's COMMAND identifies the
// knomit bridge. This is the certain signal: it names the actual process and it
// survives the config key being derived per scope. `.exe` is trimmed because the
// Makefile builds a GOOS=windows target, where the command is kb.exe
// and a bare basename comparison would miss every Windows install.
func IsKnomitCommand(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "kb"
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

// UnboundNote is the one line both hosts' hooks inject when the knomit entry is
// unbound: there is no repo to read context from, so the agent is told how to
// bind instead.
const UnboundNote = "knomit is connected unbound: call knomit_repos, then knomit_bind, " +
	"and pass the returned binding on every knomit tool call."

// Scope is what one knomit entry's args configure. RepoFlag and LensFlag
// record that the flag APPEARED, value or not: a flag with a missing or empty
// value is a BROKEN entry, never an unbound one — the bridge exits 2 on it —
// and callers must never fall back to a directory basename for it.
//
// When --lens appears it wins, and the repo fields are cleared: a stray --repo
// must never demote a lens-configured entry to a repo scope.
type Scope struct {
	Repo, Lens         string
	RepoFlag, LensFlag bool
}

// Unbound reports that neither flag appeared: the bridge proxies to the
// unscoped endpoint and the agent binds with knomit_bind. It is decided from
// the ARGS alone — never from the config key, which may carry a directory name
// (UnboundServerKey), and never from the directory.
func (s Scope) Unbound() bool { return !s.RepoFlag && !s.LensFlag }

// String names the scope for a person: "repo x", "lens y" or "unbound".
func (s Scope) String() string {
	switch {
	case s.LensFlag:
		return "lens " + s.Lens
	case s.RepoFlag:
		return "repo " + s.Repo
	}
	return "unbound"
}

// ClassifyArgs is the ONE parser of a knomit MCP entry's args. It reads them the
// way the bridge will run them:
//
//   - --log (either form) is skipped with its value, because the bridge peels
//     it before flag.Parse.
//   - Parsing STOPS at the first positional argument, a lone "-", or "--" —
//     where the bridge's flag.Parse stops. `["http://host:8080", "--repo", "x"]`
//     runs unbound, so it IS unbound.
//   - --lens wins regardless of order.
//   - A flag-shaped token is never consumed as a repo NAME: `--repo --repo x`
//     must not bind to a repo literally named "--repo".
//
// Both `-flag value` and `--flag=value` forms are accepted, since these files
// are hand-editable.
func ClassifyArgs(args []string) Scope {
	var s Scope
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-" || !strings.HasPrefix(a, "-"):
			return s
		case a == "--log" || a == "-log":
			i++
		case a == "--lens" || a == "-lens":
			if i+1 < len(args) {
				return Scope{Lens: args[i+1], LensFlag: true}
			}
			return Scope{LensFlag: true} // lens token, no value: lens mode, unusable
		case strings.HasPrefix(a, "--lens=") || strings.HasPrefix(a, "-lens="):
			_, v, _ := strings.Cut(a, "=")
			return Scope{Lens: v, LensFlag: true}
		case a == "--repo" || a == "-repo":
			s.RepoFlag = true
			if s.Repo == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				s.Repo = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "-repo="):
			s.RepoFlag = true
			if s.Repo == "" {
				_, s.Repo, _ = strings.Cut(a, "=")
			}
		}
	}
	return s
}

// Skip reasons SingleScope returns, shared by both hosts' hooks.
const (
	SkipUnbound      = "unbound"
	SkipNoBinding    = "no_binding"
	SkipAmbiguous    = "ambiguous_binding"
	SkipLensUnusable = "lens_unusable"
	SkipInvalidScope = "invalid_scope"
)

// SingleScope reduces a config's knomit entries (each entry's args) to the one
// scope they configure, or a skip reason. It is the single rule both hosts use:
// for what the hooks bind to, and for whether a flagless init keeps what is
// there — only one USABLE scope is kept.
//
// Every entry must agree: two entries naming the same scope are fine (that is
// what a duplicated-but-consistent edit looks like), but two different targets
// — including an unbound entry beside a bound one — have no principled answer.
// valid re-checks a name on READ (repos.IsValidName): the file is hand-editable
// and the name becomes an API path segment.
func SingleScope(entries [][]string, valid func(string) bool) (Scope, string) {
	if len(entries) == 0 {
		return Scope{}, SkipNoBinding
	}
	first := ClassifyArgs(entries[0])
	for _, args := range entries[1:] {
		if ClassifyArgs(args) != first {
			return Scope{}, SkipAmbiguous
		}
	}
	switch {
	case first.Unbound():
		return Scope{}, SkipUnbound
	case first.LensFlag && first.Lens == "":
		return Scope{}, SkipLensUnusable
	case first.LensFlag && !valid(first.Lens):
		return Scope{}, SkipInvalidScope
	case first.LensFlag:
		return first, ""
	case first.Repo == "":
		return Scope{}, SkipNoBinding
	case !valid(first.Repo):
		return Scope{}, SkipInvalidScope
	}
	return first, ""
}

// ReplacedText says, for a person, what a flagless init replaced with an
// unbound entry, given the skip reason SingleScope returned for it.
func ReplacedText(skip string) string {
	switch skip {
	case SkipAmbiguous:
		return "entries naming more than one scope"
	case SkipLensUnusable:
		return "--lens with no value"
	case SkipNoBinding:
		return "a knomit entry naming no usable scope"
	case SkipInvalidScope:
		return "an invalid repo or lens name"
	}
	return skip
}
