package claude

import "fmt"

// Run dispatches the `claude <subcommand>` arguments.
func Run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge claude <subcommand> (init | hook)")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "hook":
		return runHook(args[1:])
	default:
		return fmt.Errorf("unknown claude subcommand %q", args[0])
	}
}
