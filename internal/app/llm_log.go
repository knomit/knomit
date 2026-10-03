package app

import (
	"errors"

	"github.com/rs/zerolog"

	"knomit/internal/llm"
)

// logLLMSetup reports, once per app.New, what the server's own LLM adapter
// came to.
//
// A missing API key (llm.ErrNoCredentials) is ONE info line and nothing else.
// It is a configuration, not a fault: without a key the server only loses its
// own synthesis jobs, while knomit_review and knomit_hypothesize hand the LLM
// work to the calling agent. Logged as two warnings it read as a failure on
// every instance of the first mission, none of which needed a key.
//
// Any other init failure keeps both warnings: that is a key or provider the
// operator configured and that does not work.
func logLLMSetup(l zerolog.Logger, adapter llm.LLMAdapter, initErr error) {
	switch {
	case adapter != nil:
		l.Info().Msg("synthesis enabled")
	case errors.Is(initErr, llm.ErrNoCredentials):
		l.Info().Err(initErr).Msg("server-side synthesis off: no LLM API key; knomit_review and knomit_hypothesize still work (the calling agent does the LLM work)")
	default:
		if initErr != nil {
			l.Warn().Err(initErr).Msg("LLM adapter init failed")
		}
		l.Warn().Msg("synthesis disabled (no LLM adapter)")
	}
}
