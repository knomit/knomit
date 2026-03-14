package llm

// Config holds LLM-related configuration.
type Config struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
}

// DefaultConfig returns LLM config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Model:    "qwen3",
		Provider: "ollama",
	}
}
