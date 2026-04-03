package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// GitConfig holds git-related configuration.
type GitConfig struct {
	Origin string `toml:"origin"`
	Serve  bool   `toml:"serve"`
	Port   string `toml:"port"`
}

// RemoteAuthConfig holds git remote authentication settings.
type RemoteAuthConfig struct {
	Token      string `toml:"token"`
	User       string `toml:"user"`
	Password   string `toml:"password"`
	SSHKey     string `toml:"ssh_key"`
	AuthMethod string `toml:"auth_method"`
}

// Config holds LLM-related configuration.
type LLMConfig struct {
	Model    string `toml:"model"`
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	Cache    bool   `toml:"cache"`
	Batch    bool   `toml:"batch"`
}

// Config is the root configuration, composed of section structs.
type Config struct {
	Home         string           `toml:"repo"`
	Host         string           `toml:"host"`
	Port         string           `toml:"port"`
	Socket       string           `toml:"socket"`
	OntologyRoot string           `toml:"ontology_root"`
	ONNXLibPath  string           `toml:"onnx_lib_path"`
	LLM          LLMConfig        `toml:"llm"`
	Remote       RemoteAuthConfig `toml:"remote"`
	Git          GitConfig        `toml:"git"`
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Home:         home + "/.knomit",
		Host:         "localhost",
		Port:         "19278",
		OntologyRoot: "kb",
		LLM: LLMConfig{
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
		},
		Git: GitConfig{Serve: true},
	}
}

// Load builds a Config by layering: defaults → TOML file → env vars.
func Load() (Config, error) {
	cfg := Defaults()

	// Resolve Home from env first (needed for TOML file discovery).
	// KNOMIT_HOME takes precedence; KNOMIT_REPO is a backward-compatible alias.
	if v := os.Getenv("KNOMIT_HOME"); v != "" {
		cfg.Home = v
	} else if v := os.Getenv("KNOMIT_REPO"); v != "" {
		cfg.Home = v
	}

	// Find and decode TOML file.
	homeBefore := cfg.Home
	if path := findConfigFile(cfg.Home); path != "" {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}
	// Restore Home — TOML cannot override it since it's the config search root.
	cfg.Home = homeBefore

	// Overlay env vars.
	envOr("KNOMIT_HOST", &cfg.Host)
	envOr("KNOMIT_PORT", &cfg.Port)
	envOr("KNOMIT_SOCKET", &cfg.Socket)
	envOr("KNOMIT_LLM_MODEL", &cfg.LLM.Model)
	envOr("KNOMIT_LLM_PROVIDER", &cfg.LLM.Provider)
	envOr("KNOMIT_API_KEY", &cfg.LLM.APIKey)
	envBoolOr("KNOMIT_LLM_CACHE", &cfg.LLM.Cache)
	envBoolOr("KNOMIT_LLM_BATCH", &cfg.LLM.Batch)
	envOr("KNOMIT_GIT_ORIGIN", &cfg.Git.Origin)
	envBoolOr("KNOMIT_GIT_SERVE", &cfg.Git.Serve)
	envOr("KNOMIT_GIT_PORT", &cfg.Git.Port)
	envOr("KNOMIT_REMOTE_TOKEN", &cfg.Remote.Token)
	envOr("KNOMIT_REMOTE_USER", &cfg.Remote.User)
	envOr("KNOMIT_REMOTE_PASSWORD", &cfg.Remote.Password)
	envOr("KNOMIT_REMOTE_SSH_KEY", &cfg.Remote.SSHKey)
	envOr("KNOMIT_REMOTE_AUTH", &cfg.Remote.AuthMethod)
	envOr("ONNXRUNTIME_SHARED_LIBRARY", &cfg.ONNXLibPath)

	// Expand tildes in path fields.
	expandTilde(&cfg.Home)
	expandTilde(&cfg.ONNXLibPath)
	expandTilde(&cfg.Remote.SSHKey)

	return cfg, nil
}

// findConfigFile looks for knomit.toml next to the binary, then in homePath.
func findConfigFile(homePath string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "knomit.toml")
		if fileExists(p) {
			return p
		}
	}
	p := filepath.Join(homePath, "knomit.toml")
	if fileExists(p) {
		return p
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func envOr(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

func envBoolOr(key string, target *bool) {
	if v := os.Getenv(key); v != "" {
		*target = v == "true"
	}
}

func expandTilde(s *string) {
	if strings.HasPrefix(*s, "~/") {
		home, _ := os.UserHomeDir()
		*s = home + (*s)[1:]
	}
}
