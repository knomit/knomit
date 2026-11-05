package llm

import "errors"

// ErrNoCredentials is what the registry returns, wrapped, when a provider's
// API key cannot be resolved (no api_key_file, api_key_env value or api_key).
// It is a CONFIGURATION, not a fault: a server without a key only loses the
// chat's model for that provider, while knomit_review and knomit_hypothesize
// hand the LLM work to the calling agent.
var ErrNoCredentials = errors.New("no API key in the environment")
