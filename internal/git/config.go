package git

// Config holds git-related configuration.
type Config struct {
	Remote bool   `toml:"remote"`
	Port   string `toml:"port"`
}

// DefaultConfig returns git config with sensible defaults.
func DefaultConfig() Config {
	return Config{}
}
