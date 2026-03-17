package git

// Config holds git-related configuration.
type Config struct {
	Origin string `toml:"origin"`
	Serve  bool   `toml:"serve"`
	Port   string `toml:"port"`
}

// DefaultConfig returns git config with sensible defaults.
func DefaultConfig() Config {
	return Config{Serve: true}
}

// RemoteAuthConfig holds git remote authentication settings.
type RemoteAuthConfig struct {
	Token      string `toml:"token"`
	User       string `toml:"user"`
	Password   string `toml:"password"`
	SSHKey     string `toml:"ssh_key"`
	AuthMethod string `toml:"auth_method"`
}
