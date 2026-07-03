package claw

import "fmt"

// Run dispatches `claw <subcommand>` arguments.
func Run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge claw <subcommand> (init)")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	default:
		return fmt.Errorf("unknown claw subcommand %q", args[0])
	}
}
