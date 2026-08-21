// Package antigravity implements the knomit integration for Google
// Antigravity (the `agy` CLI and the Antigravity IDE).
//
// Unlike the Claude Code host, everything is scaffolded into a single owned
// plugin directory — .agents/plugins/knomit/ — so nothing is merge-required
// and the hook binds to its project by reading the mcp_config.json sitting
// next to its own hooks.json.
package antigravity

import "fmt"

// Run dispatches the `antigravity <subcommand>` arguments.
func Run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge antigravity <subcommand> (init | hook)")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "hook":
		return runHook(args[1:])
	default:
		return fmt.Errorf("unknown antigravity subcommand %q (valid: init, hook)", args[0])
	}
}
