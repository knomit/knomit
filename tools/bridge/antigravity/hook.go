package antigravity

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
)

func runHook(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: knomit-bridge antigravity hook <event> (pre-invocation)")
	}
	event := args[0]
	log.Info().Str("event", event).Msg("hook fired")
	switch event {
	case "pre-invocation":
		return hookPreInvocation(os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown hook event %q (valid: pre-invocation)", event)
	}
}
