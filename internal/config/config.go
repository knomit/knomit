package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"knomit/internal/git"
	"knomit/internal/llm"
)

// RemoteConfig holds git remote authentication settings.
type RemoteConfig struct {
	Token      string `toml:"token"`
	User       string `toml:"user"`
	Password   string `toml:"password"`
	SSHKey     string `toml:"ssh_key"`
	AuthMethod string `toml:"auth_method"`
}

// Config is the root configuration, composed of section structs.
type Config struct {
	RepoPath     string       `toml:"repo"`
	CacheDir     string       `toml:"cache_dir"`
	Port         string       `toml:"port"`
	OntologyRoot string       `toml:"ontology_root"`
	ONNXLibPath  string       `toml:"onnx_lib_path"`
	LLM          llm.Config   `toml:"llm"`
	Remote       RemoteConfig `toml:"remote"`
	Git          git.Config   `toml:"git"`
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		RepoPath:     home + "/.knomit",
		CacheDir:     home + "/.cache/knomit",
		Port:         "3000",
		OntologyRoot: "kb",
		LLM:          llm.DefaultConfig(),
		Git:          git.DefaultConfig(),
	}
}

// Load builds a Config by layering: defaults → TOML file → env vars.
func Load() (Config, error) {
	cfg := Defaults()

	// Resolve RepoPath from env first (needed for TOML file discovery).
	if v := os.Getenv("KNOMIT_REPO"); v != "" {
		cfg.RepoPath = v
	}

	// Find and decode TOML file.
	if path := findConfigFile(cfg.RepoPath); path != "" {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	// Overlay env vars.
	envOr("KNOMIT_REPO", &cfg.RepoPath)
	envOr("KNOMIT_CACHE_DIR", &cfg.CacheDir)
	envOr("KNOMIT_PORT", &cfg.Port)
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
	expandTilde(&cfg.RepoPath)
	expandTilde(&cfg.CacheDir)
	expandTilde(&cfg.ONNXLibPath)
	expandTilde(&cfg.Remote.SSHKey)

	return cfg, nil
}

// findConfigFile looks for knomit.toml next to the binary, then in repoPath.
func findConfigFile(repoPath string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "knomit.toml")
		if fileExists(p) {
			return p
		}
	}
	p := filepath.Join(repoPath, "knomit.toml")
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
