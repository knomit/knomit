package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
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

// EmbeddingsConfig selects the embedding model (by registry id).
type EmbeddingsConfig struct {
	Model string `toml:"model"`
}

// ClusterCacheConfig governs the Louvain-cluster cache: how long to wait for
// activity to settle before a background recompute, how often the checker
// wakes, and how many concurrent recomputes are allowed across all
// repos/branches. CheckInterval=="0" or "0s" disables the background
// checker entirely (read-path stale-then-refresh still applies).
type ClusterCacheConfig struct {
	QuietThreshold string `toml:"quiet_threshold"`
	CheckInterval  string `toml:"check_interval"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	// Resolution is the Louvain γ: higher = more, smaller communities. Default
	// 2.0 (was a hardcoded 1.0) — breaks over-large communities. MinCommunitySize
	// relabels communities smaller than this as noise. Both must match between the
	// background checker and the read path or the cluster cache thrashes (the cache
	// is keyed on (branch, resolution, min_community_size)).
	Resolution       float64 `toml:"resolution"`
	MinCommunitySize int     `toml:"min_community_size"`
}

// SessionConfig governs the ephemeral session database's idle reaper. Tool
// paging cursors and pipeline work-stealing sessions live there; the reaper
// deletes a session once it has been idle (no page/work-item access) longer
// than its TTL. ToolIdleTTL covers short-lived query/explain cursors;
// PipelineIdleTTL is longer because review/hypothesize loops can pause between
// work-steal calls. The reaper is never disabled: the relocated session tables
// have no other GC, so an empty or non-positive value for any knob falls back
// to its default rather than turning the sweep off.
type SessionConfig struct {
	ToolIdleTTL     string `toml:"tool_idle_ttl"`
	PipelineIdleTTL string `toml:"pipeline_idle_ttl"`
	SweepInterval   string `toml:"sweep_interval"`
}

// Config is the root configuration, composed of section structs.
type Config struct {
	Home                string             `toml:"repo"`
	Host                string             `toml:"host"`
	Port                string             `toml:"port"`
	Socket              string             `toml:"socket"`
	OntologyRoot        string             `toml:"ontology_root"`
	ONNXLibPath         string             `toml:"onnx_lib_path"`
	MethodologyMinScore float64            `toml:"methodology_min_score"`
	ClusterCache        ClusterCacheConfig `toml:"cluster_cache"`
	Session             SessionConfig      `toml:"session"`
	Embeddings          EmbeddingsConfig   `toml:"embeddings"`
	LLM                 LLMConfig          `toml:"llm"`
	Remote              RemoteAuthConfig   `toml:"remote"`
	Git                 GitConfig          `toml:"git"`
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Home:                home + "/.knomit",
		Host:                "localhost",
		Port:                "19278",
		OntologyRoot:        "kb",
		MethodologyMinScore: 0.15,
		ClusterCache: ClusterCacheConfig{
			QuietThreshold:   "10s",
			CheckInterval:    "5s",
			MaxConcurrent:    1,
			Resolution:       2.0,
			MinCommunitySize: 2,
		},
		Session: SessionConfig{
			ToolIdleTTL:     "15m",
			PipelineIdleTTL: "60m",
			SweepInterval:   "5m",
		},
		Embeddings: EmbeddingsConfig{Model: "embeddinggemma"},
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
	envOr("KNOMIT_EMBED_MODEL", &cfg.Embeddings.Model)
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
	envOr("KNOMIT_CLUSTER_CACHE_QUIET_THRESHOLD", &cfg.ClusterCache.QuietThreshold)
	envOr("KNOMIT_CLUSTER_CACHE_CHECK_INTERVAL", &cfg.ClusterCache.CheckInterval)
	envOr("KNOMIT_SESSION_TOOL_IDLE_TTL", &cfg.Session.ToolIdleTTL)
	envOr("KNOMIT_SESSION_PIPELINE_IDLE_TTL", &cfg.Session.PipelineIdleTTL)
	envOr("KNOMIT_SESSION_SWEEP_INTERVAL", &cfg.Session.SweepInterval)
	for _, err := range []error{
		envIntOr("KNOMIT_CLUSTER_CACHE_MAX_CONCURRENT", &cfg.ClusterCache.MaxConcurrent),
		envFloatOr("KNOMIT_CLUSTER_CACHE_RESOLUTION", &cfg.ClusterCache.Resolution),
		envIntOr("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", &cfg.ClusterCache.MinCommunitySize),
		envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &cfg.MethodologyMinScore),
	} {
		if err != nil {
			return Config{}, err
		}
	}

	// Expand tildes in path fields.
	expandTilde(&cfg.Home)
	expandTilde(&cfg.ONNXLibPath)
	expandTilde(&cfg.Remote.SSHKey)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the config is internally consistent. Called from
// Load so that a misconfigured TOML or env var (notably an empty
// ontology_root) surfaces at boot rather than later as silently-dropped
// synthesize outputs.
func (c Config) Validate() error {
	if strings.TrimSpace(c.OntologyRoot) == "" {
		return fmt.Errorf("config: ontology_root must not be empty")
	}
	// Composite methodology score is bounded to [0, 1] (0.6·vec + 0.4·tag,
	// each in [0,1]). NaN, negatives, or values >1 silently break filtering
	// — fail at boot instead.
	if math.IsNaN(c.MethodologyMinScore) || c.MethodologyMinScore < 0 || c.MethodologyMinScore > 1 {
		return fmt.Errorf("config: methodology_min_score must be in [0, 1], got %v", c.MethodologyMinScore)
	}
	return nil
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

// envIntOr overlays an int env var. A set-but-malformed value is an error
// surfaced at boot rather than silently ignored (which would leave the default
// in place and give no signal that the override was dropped).
func envIntOr(key string, target *int) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("config: %s must be an integer, got %q", key, v)
	}
	*target = n
	return nil
}

// envFloatOr overlays a float env var, erroring at boot on a malformed value
// for the same reason as envIntOr.
func envFloatOr(key string, target *float64) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("config: %s must be a number, got %q", key, v)
	}
	*target = f
	return nil
}

func expandTilde(s *string) {
	if strings.HasPrefix(*s, "~/") {
		home, _ := os.UserHomeDir()
		*s = home + (*s)[1:]
	}
}
