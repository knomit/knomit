package llm

// Config holds LLM-related configuration.
type Config struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	Cache    bool   `toml:"cache"`
	Batch    bool   `toml:"batch"`
}

// DefaultConfig returns LLM config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Model:    "gemini-2.5-flash",
		Provider: "gemini",
	}
}
