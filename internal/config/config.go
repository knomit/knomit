package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/rs/zerolog"
)

// DefaultRepoName is the name of the default repository/knowledge base that
// knomit creates and opens on first run when no repo is specified. Its on-disk
// database lives at <home>/repos/<DefaultRepoName>.db. This is distinct from
// the MCP server name and the git committer identity, which are both "knomit".
const DefaultRepoName = "core"

// GitConfig holds git-related configuration.
type GitConfig struct {
	Origin string `toml:"origin"`
	Serve  bool   `toml:"serve"`
	Port   string `toml:"port"`
	// NetworkTimeout bounds every remote git network operation (clone, fetch,
	// push, ls-remote). A stalled remote that accepts the connection but never
	// answers would otherwise hang the operation forever. The store derives a
	// deadline-bounded context from this value at each go-git call site.
	// Default 120s (set in Defaults). 0 disables the bound entirely (the old,
	// hang-forever behavior) — only when explicitly set to 0.
	NetworkTimeout time.Duration `toml:"network_timeout"`
}

// RemoteAuthConfig holds git remote authentication settings.
type RemoteAuthConfig struct {
	Token      string `toml:"token"`
	User       string `toml:"user"`
	Password   string `toml:"password"`
	SSHKey     string `toml:"ssh_key"`
	AuthMethod string `toml:"auth_method"`
	// KnownHosts is the OpenSSH known_hosts file used to verify SSH remote host
	// keys (trust-on-first-use; a changed key is rejected). Defaults to
	// <Home>/known_hosts. Point it at ~/.ssh/known_hosts to share the pins with
	// your interactive ssh client.
	KnownHosts string `toml:"known_hosts"`
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

// LogConfig controls logging output. The default is collector-friendly:
// human-readable console on stderr, no file. Containers set Format="json"
// (stdout, captured by the log driver); a non-container central deployment may
// set File to enable an app-managed rotating file sink.
type LogConfig struct {
	// Format is "console" (human, stderr — default) or "json" (structured).
	Format string `toml:"format"`
	// Level is a zerolog level name (trace/debug/info/warn/error/...).
	Level string `toml:"level"`
	// File, when non-empty, adds a rotating JSON file sink (lumberjack).
	// Empty (the default) logs only to stdout/stderr — the right choice for
	// containers, where the log driver owns rotation.
	File       string `toml:"file"`
	MaxSizeMB  int    `toml:"max_size_mb"`
	MaxBackups int    `toml:"max_backups"`
	MaxAgeDays int    `toml:"max_age_days"`
	// SlowRequestMS logs any HTTP/MCP request slower than this at WARN.
	// 0 disables the slow-request log.
	SlowRequestMS int `toml:"slow_request_ms"`
	// CrashFile, when non-empty, redirects fd 2 (stderr) to this append-only
	// file at startup so runtime/CGO fatal tracebacks — which bypass the
	// logger and write directly to fd 2 — are persisted. Off by default;
	// leave off in containers (the log driver already captures fd 2).
	CrashFile string `toml:"crash_file"`
}

// ClusterCacheConfig governs scoped Louvain clustering granularity. The review
// path runs Louvain over a bounded per-review subgraph in-process
// (internal/synthesize), so there is no background warmer or persisted cache to
// configure — only the algorithm's resolution and minimum community size. The
// section name stays [cluster_cache] for config back-compat.
type ClusterCacheConfig struct {
	// Resolution is the Louvain γ: higher = more, smaller communities. Default
	// 4.0 — calibrated for the SIMILAR_TO-only review subgraph clustered by gonum
	// so it yields review-sized communities matching the prior global-Louvain
	// granularity. MinCommunitySize relabels communities smaller than this as noise.
	Resolution       float64 `toml:"resolution"`
	MinCommunitySize int     `toml:"min_community_size"`
}

// DiscoveryConfig tunes the emergent-fact discovery engine (effort dial,
// verification gates, structural bridge definition). All knobs have safe
// defaults that match the design spec; per-repo overrides are a follow-on.
type DiscoveryConfig struct {
	// EffortDefault is what an absent 'effort' argument resolves to when the
	// MCP review/hypothesize tools are invoked. Vocabulary: "normal" |
	// "medium" | "high". Empty defaults to "normal" — the byte-identical-
	// pre-discovery invariant.
	EffortDefault string `toml:"effort_default"`
	// ConfidenceThreshold is the minimum confidence a discovered proposal
	// must carry to be written. Comparison is ≥; threshold-ε is rejected.
	ConfidenceThreshold float64 `toml:"confidence_threshold"`
	// BlastRadiusThreshold is the minimum BlastRadius a backward (keystone)
	// proposal's seed-anchor must transitively reach to be written. 0
	// disables the gate.
	BlastRadiusThreshold int `toml:"blast_radius_threshold"`
	// Bridge selects which structural tokens count as a bridge: "domain",
	// "entity", or "both" (default).
	Bridge string `toml:"bridge"`
	// CohFloor is the minimum intra-cluster cohesion (SIMILAR_TO edge density)
	// a bridge seed set must have to pass the quality gate. Default 0.5.
	CohFloor float64 `toml:"coh_floor"`
	// MaxMembers is the maximum number of members in a bridge seed set that
	// will be scored. Sets larger than this are rejected by the gate. Default 5.
	MaxMembers int `toml:"max_members"`
	// QualityFloor is the minimum weighted quality score Q a bridge seed set
	// must achieve to be kept. 0.0 disables the floor (all gate-passing sets
	// are kept). Default 0.0 — tuned via the calibrate tool. Default 0.0.
	QualityFloor float64 `toml:"quality_floor"`
	// WCoh is the weight applied to the cohesion component in Q. Default 1.0.
	WCoh float64 `toml:"w_coh"`
	// WGap is the weight applied to the derivation-gap component in Q. Default 1.0.
	WGap float64 `toml:"w_gap"`
	// WSpec is the weight applied to the specificity component in Q. Default 1.0.
	WSpec float64 `toml:"w_spec"`
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
	Home         string `toml:"repo"`
	Host         string `toml:"host"`
	Port         string `toml:"port"`
	Socket       string `toml:"socket"`
	OntologyRoot string `toml:"ontology_root"`
	ONNXLibPath  string `toml:"onnx_lib_path"`
	// LocalOriginRoot is the filesystem directory under which local-path git
	// origins (bare absolute paths or file:// URLs) are permitted. Empty
	// (the default) disables local-path origins entirely: the web layer
	// rejects any non-network origin. Set via [local_origin_root] in TOML or
	// KNOMIT_LOCAL_ORIGIN_ROOT.
	LocalOriginRoot string `toml:"local_origin_root"`
	// ReadOnly turns the instance into a read-only demo: all mutating HTTP
	// methods are rejected (403), the /git endpoint and MCP write tools are
	// not exposed, and origin sync is pull-only (fetch, never push). Set via
	// [read_only] in TOML or KNOMIT_READ_ONLY. Startup-only.
	ReadOnly            bool               `toml:"read_only"`
	MethodologyMinScore float64            `toml:"methodology_min_score"`
	ClusterCache        ClusterCacheConfig `toml:"cluster_cache"`
	Session             SessionConfig      `toml:"session"`
	Discovery           DiscoveryConfig    `toml:"discovery"`
	Embeddings          EmbeddingsConfig   `toml:"embeddings"`
	LLM                 LLMConfig          `toml:"llm"`
	Remote              RemoteAuthConfig   `toml:"remote"`
	Git                 GitConfig          `toml:"git"`
	Log                 LogConfig          `toml:"log"`
	Runtime             RuntimeConfig      `toml:"runtime"`
}

// RuntimeConfig configures the optional runtime diagnostics port (live
// introspection + pprof + metrics). Off unless Addr is set; bind it to a local
// address only — it is never meant to face the network.
type RuntimeConfig struct {
	Addr string `toml:"addr"`
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
			Resolution:       4.0,
			MinCommunitySize: 2,
		},
		Session: SessionConfig{
			ToolIdleTTL:     "15m",
			PipelineIdleTTL: "60m",
			SweepInterval:   "5m",
		},
		Discovery: DiscoveryConfig{
			EffortDefault:        "normal",
			ConfidenceThreshold:  0.5,
			BlastRadiusThreshold: 1,
			Bridge:               "both",
			CohFloor:             0.5,
			MaxMembers:           5,
			QualityFloor:         0.0,
			WCoh:                 1.0,
			WGap:                 1.0,
			WSpec:                1.0,
		},
		Embeddings: EmbeddingsConfig{Model: "embeddinggemma"},
		LLM: LLMConfig{
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
		},
		Git: GitConfig{Serve: true, NetworkTimeout: 120 * time.Second},
		Log: LogConfig{
			Format:        "console",
			Level:         "info",
			MaxSizeMB:     10,
			MaxBackups:    3,
			MaxAgeDays:    7,
			SlowRequestMS: 1000,
		},
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
	if err := envDurationOr("KNOMIT_GIT_NETWORK_TIMEOUT", &cfg.Git.NetworkTimeout); err != nil {
		return Config{}, err
	}
	envOr("KNOMIT_REMOTE_TOKEN", &cfg.Remote.Token)
	envOr("KNOMIT_REMOTE_USER", &cfg.Remote.User)
	envOr("KNOMIT_REMOTE_PASSWORD", &cfg.Remote.Password)
	envOr("KNOMIT_REMOTE_SSH_KEY", &cfg.Remote.SSHKey)
	envOr("KNOMIT_REMOTE_AUTH", &cfg.Remote.AuthMethod)
	envOr("KNOMIT_REMOTE_KNOWN_HOSTS", &cfg.Remote.KnownHosts)
	envOr("KNOMIT_LOCAL_ORIGIN_ROOT", &cfg.LocalOriginRoot)
	envBoolOr("KNOMIT_READ_ONLY", &cfg.ReadOnly)
	envOr("ONNXRUNTIME_SHARED_LIBRARY", &cfg.ONNXLibPath)
	envOr("KNOMIT_SESSION_TOOL_IDLE_TTL", &cfg.Session.ToolIdleTTL)
	envOr("KNOMIT_SESSION_PIPELINE_IDLE_TTL", &cfg.Session.PipelineIdleTTL)
	envOr("KNOMIT_SESSION_SWEEP_INTERVAL", &cfg.Session.SweepInterval)
	envOr("KNOMIT_DISCOVERY_EFFORT_DEFAULT", &cfg.Discovery.EffortDefault)
	envOr("KNOMIT_DISCOVERY_BRIDGE", &cfg.Discovery.Bridge)
	envOr("KNOMIT_LOG_FORMAT", &cfg.Log.Format)
	envOr("KNOMIT_LOG_LEVEL", &cfg.Log.Level)
	envOr("KNOMIT_LOG_FILE", &cfg.Log.File)
	envOr("KNOMIT_CRASH_LOG", &cfg.Log.CrashFile)
	envOr("KNOMIT_RUNTIME_ADDR", &cfg.Runtime.Addr)
	for _, err := range []error{
		envFloatOr("KNOMIT_CLUSTER_CACHE_RESOLUTION", &cfg.ClusterCache.Resolution),
		envIntOr("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", &cfg.ClusterCache.MinCommunitySize),
		envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &cfg.MethodologyMinScore),
		envFloatOr("KNOMIT_DISCOVERY_CONFIDENCE_THRESHOLD", &cfg.Discovery.ConfidenceThreshold),
		envIntOr("KNOMIT_DISCOVERY_BLAST_RADIUS_THRESHOLD", &cfg.Discovery.BlastRadiusThreshold),
		envIntOr("KNOMIT_LOG_MAX_SIZE", &cfg.Log.MaxSizeMB),
		envIntOr("KNOMIT_LOG_MAX_BACKUPS", &cfg.Log.MaxBackups),
		envIntOr("KNOMIT_LOG_MAX_AGE", &cfg.Log.MaxAgeDays),
		envIntOr("KNOMIT_LOG_SLOW_MS", &cfg.Log.SlowRequestMS),
	} {
		if err != nil {
			return Config{}, err
		}
	}

	// Expand tildes in path fields.
	expandTilde(&cfg.Home)
	expandTilde(&cfg.ONNXLibPath)
	expandTilde(&cfg.Remote.SSHKey)
	expandTilde(&cfg.Remote.KnownHosts)
	expandTilde(&cfg.LocalOriginRoot)

	// Default known_hosts to <Home>/known_hosts. Resolved after tilde expansion
	// so it inherits the already-expanded Home, and only when unset so an
	// explicit TOML/env value (e.g. ~/.ssh/known_hosts) wins.
	if cfg.Remote.KnownHosts == "" {
		cfg.Remote.KnownHosts = filepath.Join(cfg.Home, "known_hosts")
	}

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
	// discovery.effort_default is consumed raw by the MCP review/hypothesize
	// handlers (it is NOT coerced like discovery.bridge), so an unknown value
	// would otherwise pass boot and then fail EVERY no-argument review /
	// hypothesize call with a confusing "invalid effort" error far from the
	// cause. Fail at boot instead. Empty is allowed: the accessor maps it to
	// "normal". Vocabulary mirrors synthesize.Effort (kept as literals to
	// avoid a config→synthesize import cycle).
	switch c.Discovery.EffortDefault {
	case "", "normal", "medium", "high":
	default:
		return fmt.Errorf("config: discovery.effort_default must be one of normal, medium, high, got %q", c.Discovery.EffortDefault)
	}
	// discovery.bridge is coerced to a safe default downstream, but validate
	// it here too so a typo fails loudly at boot rather than silently widening
	// the bridge axis to "both". Empty is allowed (accessor maps it to "both").
	switch c.Discovery.Bridge {
	case "", "domain", "entity", "both":
	default:
		return fmt.Errorf("config: discovery.bridge must be one of domain, entity, both, got %q", c.Discovery.Bridge)
	}
	// discovery.confidence_threshold gates how selective the discovery engine
	// is. 0 is valid (disables the gate). Values outside [0, 1] are nonsensical
	// because fact confidence is always in [0, 1].
	if math.IsNaN(c.Discovery.ConfidenceThreshold) || c.Discovery.ConfidenceThreshold < 0 || c.Discovery.ConfidenceThreshold > 1 {
		return fmt.Errorf("config: discovery.confidence_threshold must be in [0, 1], got %v", c.Discovery.ConfidenceThreshold)
	}
	// discovery.blast_radius_threshold is the minimum live-dependent count a
	// backward keystone must clear. 0 is valid (disables the gate). A negative
	// value would silently disable it the same way 0 does but with no documented
	// meaning — a typo that should fail loudly at boot, like confidence_threshold.
	if c.Discovery.BlastRadiusThreshold < 0 {
		return fmt.Errorf("config: discovery.blast_radius_threshold must be >= 0, got %d", c.Discovery.BlastRadiusThreshold)
	}
	// log.format selects the sink encoding; an unknown value would silently
	// fall through to console — fail at boot instead. Empty maps to console.
	switch c.Log.Format {
	case "", "console", "json":
	default:
		return fmt.Errorf("config: log.format must be console or json, got %q", c.Log.Format)
	}
	// log.level is parsed by zerolog; a typo (e.g. "loud") would otherwise be
	// dropped and leave the level at its zero value (trace) silently.
	if c.Log.Level != "" {
		if _, err := zerolog.ParseLevel(c.Log.Level); err != nil {
			return fmt.Errorf("config: log.level %q is not a valid level: %w", c.Log.Level, err)
		}
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

// envDurationOr overlays a Go duration env var (e.g. "120s", "2m", "0").
// A set-but-malformed value errors at boot like envIntOr/envFloatOr rather
// than silently leaving the default in place. "0" is valid and means "no
// network timeout" (preserve hang-forever behavior).
func envDurationOr(key string, target *time.Duration) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("config: %s must be a Go duration (e.g. 120s, 2m), got %q", key, v)
	}
	*target = d
	return nil
}

func expandTilde(s *string) {
	if strings.HasPrefix(*s, "~/") {
		home, _ := os.UserHomeDir()
		*s = home + (*s)[1:]
	}
}
