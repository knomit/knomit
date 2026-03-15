package git

// Config holds git-related configuration.
type Config struct {
	Origin string `toml:"origin"`
	Serve  bool   `toml:"serve"`
	Port   string `toml:"port"`
}

// DefaultConfig returns git config with sensible defaults.
func DefaultConfig() Config {
	return Config{}
}
