package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/embeddings/params"
)

// GitConfig holds git-related configuration.
//
// There is deliberately no server-level `origin` here. An origin belongs to a
// repo, not to the server: knomit serves zero or more repos, none of them
// privileged, so a single URL in the server config has no unambiguous target.
// Origins are attached per-repo at creation (CreateSpec.Origin) or afterwards
// via PUT /api/v1/{repo}/origin.
type GitConfig struct {
	Serve bool   `toml:"serve"`
	Port  string `toml:"port"`
	// NetworkTimeout bounds every remote git network operation (clone, fetch,
	// push, ls-remote). A stalled remote that accepts the connection but never
	// answers would otherwise hang the operation forever. The store derives a
	// deadline-bounded context from this value at each go-git call site.
	// Default 120s (set in Defaults). 0 disables the bound entirely (the old,
	// hang-forever behavior) — only when explicitly set to 0.
	NetworkTimeout time.Duration `toml:"network_timeout"`
	// MaxProbeBytes bounds what the pre-create initialization probe will pull
	// into MEMORY to answer "does this branch carry an ontology?".
	//
	// That probe clones a tip (Depth 1, single branch) into a memory store from
	// a caller-supplied URL. Depth bounds the history, not the content, so the
	// whole tip tree lands on the heap — for a question that needs one tree
	// entry. A remote past this budget is reported as NOT ESTABLISHED rather
	// than answered, because an answer nobody could obtain must not route a
	// create. Default 64 MiB (set in Defaults); 0 disables the bound.
	MaxProbeBytes int64 `toml:"max_probe_bytes"`
	// LocalReconcileInterval is how often a repo with NO origin fast-forwards
	// its consensus branch (main) from its own agent branch, so that branch
	// means the same thing here as on an origin-backed host and a peer
	// subscribing to this instance sees the facts.
	//
	// A tick that finds the two tips equal does nothing, so a quiet repo costs
	// two ref reads; a burst of writes costs one advance. Default 30s (set in
	// Defaults). 0 disables the local reconcile entirely.
	LocalReconcileInterval time.Duration `toml:"local_reconcile_interval"`
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
	// MaxBatchTokens bounds the PADDED token count — rows × the longest row —
	// fed to one ONNX session.Run. Memory scales with that product, not with
	// the document count: at a fixed 16384 padded tokens the measured peak is
	// the same (within 1.3%) whether it is 128×128, 64×256 or 256×64, while a
	// fixed count of 32 documents ranged from 283 MiB to 9030 MiB on document
	// length alone. That 9030 MiB run is what OOM-killed the server.
	//
	// 0 (the default) means auto: the app layer derives a value from this
	// machine's memory ceiling — a cgroup limit when one exists, otherwise
	// physical RAM. Deriving CLAMPS DOWN ONLY, so auto never exceeds
	// embeddings.DefaultMaxBatchTokens; a small host or a memory-capped
	// container gets less, a large machine gets the same as everyone else.
	// An undetectable ceiling falls back to that default rather than failing.
	//
	// Set an explicit value to opt out of derivation — a configured number is
	// used verbatim. (Detection still runs; its result is logged and then
	// ignored for the budget, which is useful for diagnosing a host.)
	MaxBatchTokens int `toml:"max_batch_tokens"`
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
	// Client-session presence thresholds (client_sessions in control.db).
	// Silent longer than ClientDeadAfter ⇒ shown as dead; longer than
	// ClientHiddenAfter ⇒ excluded from the presence view; rows whose
	// last_seen_at is older than ClientRetention are purged by the reaper.
	// "0" retention disables purging (warned once at boot).
	ClientDeadAfter   string `toml:"client_dead_after"`
	ClientHiddenAfter string `toml:"client_hidden_after"`
	ClientRetention   string `toml:"client_retention"`
}

// ExperimentsConfig configures the experiment lifecycle: an `exp/<name>`
// branch forked from this instance's agent branch, worked on in isolation,
// then committed back or thrown away.
type ExperimentsConfig struct {
	// ExpiryDays is how long an experiment may sit with no commit on it
	// before the sweeper rolls it back. Default 30. 0 means NEVER EXPIRE, and
	// then the sweeper does not run at all — an experiment nobody can lose is
	// a legitimate choice, and a loop that ticks forever to decide nothing is
	// not.
	//
	// Days, not a Duration, because this is a human-scale retention policy a
	// person sets in the settings UI, and "30" is what they mean.
	//
	// This is the ONLY experiment setting. How often the sweeper looks is not
	// a policy question and is not exposed: it is a package default in
	// internal/repos, passed as a parameter so the loop's error path stays
	// testable. One control, one decision.
	ExpiryDays int `toml:"expiry_days"`
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
	Experiments         ExperimentsConfig  `toml:"experiments"`
	Embeddings          EmbeddingsConfig   `toml:"embeddings"`
	LLM                 LLMConfig          `toml:"llm"`
	Remote              RemoteAuthConfig   `toml:"remote"`
	Git                 GitConfig          `toml:"git"`
	Log                 LogConfig          `toml:"log"`
	Runtime             RuntimeConfig      `toml:"runtime"`
	Auth                AuthConfig         `toml:"auth"`
}

// AuthConfig governs who may do what on this instance (F19 phase 1).
//
// The model in one line: every request gets an auth.Principal at the edge,
// and a mutation needs the `write` permission that principal holds. There are
// two ways to be somebody today --
//
//	a unix socket peer   the kernel reports the uid, and the principal is
//	                     bridge:uid:<n>@socket. Boot seeds the SERVER's own
//	                     uid with LoopbackDefault, so the local bridge works
//	                     with no configuration.
//	nobody, on loopback  the anonymous principal, holding LoopbackDefault,
//	                     while Require is false.
//
// Certificates and bearer tokens are phases 2 and 3 and produce the same
// Principal. Six permissions exist (read, write, push:own, merge:main,
// operator, admin) but only `write` is ENFORCED in phase 1 -- do not read the
// presence of the others as protection.
//
// Refusals are 403, never 401: RFC 7235 makes WWW-Authenticate mandatory on a
// 401 and phase 1 has no scheme a TCP caller could satisfy. They are told
// apart by title -- "Authentication required" when there is no principal,
// "Permission denied" when there is one and it lacks the permission.
//
// An upgrade changes nothing: Require defaults false and LoopbackDefault is
// everything a local operator could already do.
type AuthConfig struct {
	// Require, when true, refuses any request that carries no verified
	// principal. False (the default) keeps loopback TCP working as it did
	// before authentication existed: such requests run as the anonymous
	// principal with LoopbackDefault's permissions. Never true by default.
	Require bool `toml:"require"`
	// LoopbackDefault lists the permissions the anonymous loopback principal
	// holds while Require is false. Names are validated by auth.ParseSet at
	// the first Handler(); a typo fails there rather than granting the wrong
	// thing. An empty (but present) list is honoured as "anonymous holds
	// nothing" -- only an absent one falls back to the defaults.
	LoopbackDefault []string `toml:"loopback_default"`
}

// RuntimeConfig configures the optional runtime diagnostics port (live
// introspection + pprof + metrics). Off unless Addr is set; bind it to a local
// address only — it is never meant to face the network.
type RuntimeConfig struct {
	Addr string `toml:"addr"`
}

// Defaults returns a Config populated with default values.
//
// Home is deliberately EMPTY here, and Load fills it in. Resolving the data
// root can fail — no %LOCALAPPDATA%, no %USERPROFILE%, no $HOME — and this
// function cannot report that, which is precisely how the old version went
// wrong: it wrote `home, _ := os.UserHomeDir()` and concatenated, so a failed
// lookup produced "/.knomit" and the OS resolved it against the current drive
// as C:\.knomit. A zero value that Load must resolve cannot silently become a
// real directory the way a concatenated empty string can.
//
// Callers that build a Config without Load (tests, tools that only want
// .Discovery) must set Home themselves if they use it.
func Defaults() Config {
	return Config{
		Host:                "localhost",
		Port:                "19278",
		OntologyRoot:        "kb",
		MethodologyMinScore: 0.15,
		ClusterCache: ClusterCacheConfig{
			Resolution:       4.0,
			MinCommunitySize: 2,
		},
		Auth: AuthConfig{
			// Everything a local operator could do before [auth] existed.
			LoopbackDefault: []string{"read", "write", "push:own", "operator", "admin"},
		},
		Session: SessionConfig{
			ToolIdleTTL:       "15m",
			PipelineIdleTTL:   "60m",
			SweepInterval:     "5m",
			ClientDeadAfter:   "1h",
			ClientHiddenAfter: "3h",
			ClientRetention:   "168h",
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
		Embeddings: EmbeddingsConfig{Model: params.DefaultModelID},
		LLM: LLMConfig{
			Model:    "gemini-2.5-flash",
			Provider: "gemini",
		},
		Experiments: ExperimentsConfig{ExpiryDays: 30},
		Git: GitConfig{
			Serve:                  true,
			NetworkTimeout:         120 * time.Second,
			MaxProbeBytes:          64 << 20,
			LocalReconcileInterval: 30 * time.Second,
		},
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
	//
	// Only when neither is set does the per-OS default apply, and a default
	// that cannot be resolved STOPS startup. Writing to a wrong-but-writable
	// path is the worse failure: it looks like a fresh install, so the models
	// download again and a second SSH identity is generated under a root
	// nobody will think to look in.
	home, err := ResolveHome()
	if err != nil {
		return Config{}, fmt.Errorf("config: cannot determine the knomit data root: %w", err)
	}
	cfg.Home = home

	// Find and decode TOML file.
	homeBefore := cfg.Home
	if path := findConfigFile(cfg.Home); path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, err
		}
		warnUndecoded(path, md.Undecoded())
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
	if err := envBoolOr("KNOMIT_LLM_CACHE", &cfg.LLM.Cache); err != nil {
		return Config{}, err
	}
	if err := envBoolOr("KNOMIT_LLM_BATCH", &cfg.LLM.Batch); err != nil {
		return Config{}, err
	}
	if err := envBoolOr("KNOMIT_GIT_SERVE", &cfg.Git.Serve); err != nil {
		return Config{}, err
	}
	envOr("KNOMIT_GIT_PORT", &cfg.Git.Port)
	if err := envDurationOr("KNOMIT_GIT_NETWORK_TIMEOUT", &cfg.Git.NetworkTimeout); err != nil {
		return Config{}, err
	}
	if err := envDurationOr("KNOMIT_GIT_LOCAL_RECONCILE_INTERVAL", &cfg.Git.LocalReconcileInterval); err != nil {
		return Config{}, err
	}
	if err := envIntOr("KNOMIT_EXPERIMENTS_EXPIRY_DAYS", &cfg.Experiments.ExpiryDays); err != nil {
		return Config{}, err
	}
	envOr("KNOMIT_REMOTE_TOKEN", &cfg.Remote.Token)
	envOr("KNOMIT_REMOTE_USER", &cfg.Remote.User)
	envOr("KNOMIT_REMOTE_PASSWORD", &cfg.Remote.Password)
	envOr("KNOMIT_REMOTE_SSH_KEY", &cfg.Remote.SSHKey)
	envOr("KNOMIT_REMOTE_AUTH", &cfg.Remote.AuthMethod)
	envOr("KNOMIT_REMOTE_KNOWN_HOSTS", &cfg.Remote.KnownHosts)
	envOr("KNOMIT_LOCAL_ORIGIN_ROOT", &cfg.LocalOriginRoot)
	if err := envBoolOr("KNOMIT_READ_ONLY", &cfg.ReadOnly); err != nil {
		return Config{}, err
	}
	envOr("ONNXRUNTIME_SHARED_LIBRARY", &cfg.ONNXLibPath)
	envOr("KNOMIT_SESSION_TOOL_IDLE_TTL", &cfg.Session.ToolIdleTTL)
	envOr("KNOMIT_SESSION_PIPELINE_IDLE_TTL", &cfg.Session.PipelineIdleTTL)
	envOr("KNOMIT_SESSION_SWEEP_INTERVAL", &cfg.Session.SweepInterval)
	envOr("KNOMIT_SESSION_CLIENT_DEAD_AFTER", &cfg.Session.ClientDeadAfter)
	envOr("KNOMIT_SESSION_CLIENT_HIDDEN_AFTER", &cfg.Session.ClientHiddenAfter)
	envOr("KNOMIT_SESSION_CLIENT_RETENTION", &cfg.Session.ClientRetention)
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
		envIntOr("KNOMIT_EMBED_MAX_BATCH_TOKENS", &cfg.Embeddings.MaxBatchTokens),
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
	for _, p := range []*string{
		&cfg.Home,
		&cfg.ONNXLibPath,
		&cfg.Remote.SSHKey,
		&cfg.Remote.KnownHosts,
		&cfg.LocalOriginRoot,
	} {
		if err := expandTilde(p); err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
	}

	// Default known_hosts to <Home>/known_hosts. Resolved after tilde expansion
	// so it inherits the already-expanded Home, and only when unset so an
	// explicit TOML/env value (e.g. ~/.ssh/known_hosts) wins.
	if cfg.Remote.KnownHosts == "" {
		cfg.Remote.KnownHosts = filepath.Join(cfg.Home, "known_hosts")
	}

	// Default the unix socket to <Home>/knomit.sock, for the same reason and
	// at the same point as known_hosts: after tilde expansion, and only when
	// nothing set it, so a TOML or KNOMIT_SOCKET value wins.
	//
	// The socket is the local bridge's credential -- the kernel tells the
	// server which uid is on the other end (internal/auth.PeerCred) -- so it
	// has to exist without being configured, and the directory mode is what
	// guards it. Windows has no AF_UNIX default here.
	if cfg.Socket == "" && runtime.GOOS != "windows" {
		cfg.Socket = filepath.Join(cfg.Home, socketFile)
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
	// experiments.expiry_days is a retention policy: 0 means never expire and
	// is a real choice, but a NEGATIVE value would make every cutoff lie in
	// the future and the first sweep would roll back every experiment in the
	// repo. That is unrecoverable (there is no archive), so it fails at boot
	// rather than on the first tick.
	if c.Experiments.ExpiryDays < 0 {
		return fmt.Errorf("config: experiments.expiry_days must be >= 0 (0 means never expire), got %d", c.Experiments.ExpiryDays)
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
	// embeddings.max_batch_tokens is a padded-token budget, so a negative value
	// is meaningless and would otherwise reach the packer, which degrades to one
	// document per inference — a silent ~30x slowdown on re-embed rather than a
	// visible failure. 0 is VALID and is the documented auto sentinel (the app
	// layer resolves it), exactly as "" means "normal" for discovery.effort_default.
	if c.Embeddings.MaxBatchTokens < 0 {
		return fmt.Errorf("config: embeddings.max_batch_tokens must be >= 0 (0 = auto), got %d", c.Embeddings.MaxBatchTokens)
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

// warnUndecoded reports every TOML key that decoded into nothing.
//
// BurntSushi's decoder drops an unknown key in silence, which makes a typo
// indistinguishable from a setting that does not work — and equally covers a key
// knomit used to read and no longer does (git.origin, say): the operator edits
// it, restarts, and nothing changes anywhere with no signal as to why.
func warnUndecoded(path string, keys []toml.Key) {
	for _, k := range keys {
		log.Warn().Str("file", path).Str("key", k.String()).Msg("unknown config key, ignored")
	}
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

// envBoolOr overlays a bool env var. Only the spellings strconv.ParseBool
// accepts (1/t/T/TRUE/true/True and 0/f/F/FALSE/false/False) are an override;
// anything else is an error surfaced at boot like envIntOr/envDurationOr.
// Treating an unrecognised value as false is what made KNOMIT_GIT_SERVE=1
// turn OFF a feature that defaults to on, with no signal that it had.
func envBoolOr(key string, target *bool) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("config: %s must be a boolean (1, t, true, 0, f, false), got %q", key, v)
	}
	*target = b
	return nil
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

// expandTilde rewrites a leading "~/" to the user's home directory.
//
// This is the OPERATING SYSTEM's notion of home, not Config.Home: a "~/" a
// person typed into knomit.toml means their home directory, and on Windows
// the data root no longer lives there at all.
//
// A "~/" that cannot be expanded is an error, not a no-op and not an empty
// prefix. Dropping the error here turned "~/.ssh/known_hosts" into
// "/.ssh/known_hosts" — the current drive's root on Windows — which is the
// same class of bug as the one that created C:\.knomit.
func expandTilde(s *string) error {
	if !strings.HasPrefix(*s, "~/") {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot expand %q: %w", *s, err)
	}
	*s = home + (*s)[1:]
	return nil
}
