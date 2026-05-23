package claude

import (
	"fmt"
	"os"
)

func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge claude hook <event>")
	}
	event := args[0]
	switch event {
	case "session-start":
		return hookSessionStart(os.Stdin, os.Stdout)
	case "post-commit":
		return hookPostCommit(os.Stdin, os.Stdout)
	case "pre-compact":
		return hookPreCompact(os.Stdin, os.Stdout)
	case "stop":
		return hookStop(os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown hook event %q (valid: session-start, post-commit, pre-compact, stop)", event)
	}
}
