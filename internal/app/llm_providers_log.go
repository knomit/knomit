package app

import (
	"github.com/rs/zerolog"

	"knomit/internal/foundation/config"
)

// logLLMProviderProblems warns once per chat provider whose api_key_env is not
// an environment variable name. The provider still loads; its models are
// unavailable with this same text. The text names the provider and never
// carries the value.
func logLLMProviderProblems(l zerolog.Logger, c config.LLMConfig) {
	for _, msg := range c.APIKeyEnvProblems() {
		l.Warn().Msg(msg)
	}
}
