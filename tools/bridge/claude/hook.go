package claude

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
)

func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge claude hook <event>")
	}
	event := args[0]
	log.Info().Str("event", event).Msg("hook fired")
	switch event {
	case "session-start":
		return hookSessionStart(os.Stdin, os.Stdout)
	case "post-edit":
		return hookPostEdit(os.Stdin, os.Stdout)
	case "pre-compact":
		return hookPreCompact(os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown hook event %q (valid: session-start, post-edit, pre-compact)", event)
	}
}
