package config

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// PipelineResumeWindow is how recently a review session must have been
	// used for a new start to resume it rather than displace it. Must be
	// positive and shorter than PipelineIdleTTL. Empty means 10m, or half of
	// PipelineIdleTTL when that is 10m or less; it is left out of Defaults so
	// a config that only shortens PipelineIdleTTL still boots.
	PipelineResumeWindow string `toml:"pipeline_resume_window"`
	SweepInterval        string `toml:"sweep_interval"`
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
	Home string `toml:"repo"`
	Host string `toml:"host"`
	Port string `toml:"port"`
	// Socket is the local authenticated listener: KNOMIT_SOCKET, else this
	// key, else a default under Home (see socketFor). An explicit value must
	// be an absolute path on unix (a leading ~ is expanded) and a pipe name
	// \\.\pipe\<name> on Windows (no ~ expansion). The server reads it once at startup, while the hooks read it
	// per connection and the MCP bridge when it starts, so a change takes
	// effect only after the server restarts — until then clients find no
	// listener at the new path and use TCP without the verified identity.
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
	TLS                 TLSConfig          `toml:"tls"`
	OAuth               OAuthConfig        `toml:"oauth"`
}

// TLSConfig is the instance-to-instance listener (F19 phase 2): mutual TLS
// for ENROLLED instances only, on its own port, beside the plaintext one,
// which does not change.
//
// Addr empty (the default) means no TLS listener. A set Addr with no
// certificate installed in Dir logs a WARN and serves plaintext only, so
// configuring the listener before `knomit identity install` is harmless.
//
// Dir holds instance.crt, root.crt, crl.pem and crl.number. The instance KEY
// is not here: it stays at [remote].ssh_key / <Home>/id_ed25519, the one copy.
type TLSConfig struct {
	Addr string `toml:"addr"` // e.g. "0.0.0.0:19279"; env KNOMIT_TLS_ADDR
	Dir  string `toml:"dir"`  // default <Home>/pki; env KNOMIT_TLS_DIR
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
	// LoopbackHosts lists extra DNS names that a loopback peer may put in its
	// Host header and still be the anonymous principal (#281). Without it,
	// a loopback TCP request whose Host is a DNS name other than localhost
	// or the bind host is refused with 421: that is what a DNS-rebound web
	// page looks like, and it would otherwise hold LoopbackDefault.
	//
	// List the name a same-host proxy forwards unchanged — code-server
	// reached over Tailscale, `tailscale serve`, a Codespaces or dev-tunnel
	// name, an nginx that keeps Host. Exact names only: no port, scheme or
	// pattern. Listing a name gives everyone who reaches knomit through it
	// LoopbackDefault, and a name is only as safe as whoever answers DNS
	// for it: never list one you do not control, and prefer a name your own
	// resolver answers over a .local mDNS name any LAN host can claim.
	//
	// Read it through EffectiveLoopbackHosts, which adds the bind host.
	// Older knomit versions only warn about the key, so it can be added
	// before upgrading.
	LoopbackHosts []string `toml:"loopback_hosts"`
}

// EffectiveLoopbackHosts is LoopbackHosts lower-cased, plus bindHost when
// that is a DNS name: the operator already named it as the address to
// serve, which is the same act as listing it. A wildcard or an IP literal
// adds nothing (IP literals pass the Host check anyway: DNS rebinding needs
// a name).
//
// bindHost must be the EFFECTIVE bind host: `knomit serve --host` is
// applied after Load (cmd/serve.go), so this is called where the Server is
// built (internal/app), never at load, or the flag would be missed.
func (a AuthConfig) EffectiveLoopbackHosts(bindHost string) []string {
	out := make([]string, 0, len(a.LoopbackHosts)+1)
	for _, h := range a.LoopbackHosts {
		out = append(out, strings.ToLower(h))
	}
	name := strings.TrimSuffix(strings.TrimPrefix(bindHost, "["), "]")
	if name != "" && net.ParseIP(name) == nil {
		out = append(out, strings.ToLower(name))
	}
	return out
}

// EffectiveLoopbackDefault is the permission list the anonymous loopback
// principal actually holds. It is the ONE place the nil fallback lives.
//
// nil means the AuthConfig was built without config — which in practice only
// tests do, since production goes through internal/app — and falls back to the
// shipped defaults. An EMPTY BUT NON-NIL slice ([auth] loopback_default = []
// in TOML) is honoured as "anonymous holds nothing": an operator who writes an
// empty list means it, and collapsing that into the fallback would silently
// grant the full default set to someone who asked for none of it.
//
// Both enforcement-side callers go through this — web.Server.grants(), which
// resolves what anonymous may do, and app.seedOwnPrincipal, which seeds the server's
// own socket uid. They used to each carry their own copy of the nil check. If
// one had ever been changed alone, boot would seed one set while the
// middleware resolved another, and the two enforcement points would disagree
// about exactly the principal this phase exists to serve.
func (a AuthConfig) EffectiveLoopbackDefault() []string {
	if a.LoopbackDefault == nil {
		return Defaults().Auth.LoopbackDefault
	}
	return a.LoopbackDefault
}

// RuntimeConfig configures the optional runtime diagnostics port (live
// introspection + pprof + metrics + process controls). Off unless Addr is set.
// It has no authentication, so Validate refuses an Addr whose host is not
// loopback — 0.0.0.0, an empty host as in ":6060", a LAN address or a DNS name
// other than localhost — unless AllowRemote is set.
type RuntimeConfig struct {
	Addr string `toml:"addr"`
	// AllowRemote permits a non-loopback Addr AND lets the port answer
	// non-loopback peers (a remote Prometheus scraper, say). Anyone who can
	// reach the address can then read pprof and flip the log level, heap
	// dumps and profilers: put it behind a firewall. Browser-made requests
	// and unlisted DNS Host names are refused either way (#288).
	AllowRemote bool `toml:"allow_remote"`
}

// addrIsLoopback reports whether a host:port listen address binds only
// loopback: "localhost" in any case, or a loopback IP literal. An empty host
// (":6060") binds every interface and is NOT loopback.
func addrIsLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
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
		// Off (no issuer, no addr). The TTLs are policy defaults.
		OAuth: OAuthConfig{
			AccessTTL:     2 * time.Hour,
			RefreshTTL:    14 * 24 * time.Hour,
			DefaultClient: true,
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
	//
	// ResolveHome (via homeAndConfig) tilde-expands the root and refuses one
	// that is still relative, BEFORE knomit.toml is looked for in it.
	// SocketPath and the bridge's credentials go through the same function, so
	// no caller can search a literal "~/..." directory or one relative to its
	// own working directory.
	home, path, err := homeAndConfig()
	if err != nil {
		return Config{}, err
	}
	cfg.Home = home
	if ignored := ignoredExecutableConfig(home); ignored != "" {
		ignoredConfigWarnOnce.Do(func() {
			log.Warn().Str("ignored", ignored).Str("read", filepath.Join(home, "knomit.toml")).
				Msg("knomit.toml beside the executable is not read; move its settings into the data root's knomit.toml")
		})
	}

	// Decode the TOML file.
	homeBefore := cfg.Home
	if path != "" {
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
	// KNOMIT_SOCKET is applied in socketFor, below — not here.
	envOr("KNOMIT_TLS_ADDR", &cfg.TLS.Addr)
	envOr("KNOMIT_TLS_DIR", &cfg.TLS.Dir)
	envOr("KNOMIT_OAUTH_ISSUER", &cfg.OAuth.Issuer)
	envOr("KNOMIT_OAUTH_ADDR", &cfg.OAuth.Addr)
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
	envOr("KNOMIT_SESSION_PIPELINE_RESUME_WINDOW", &cfg.Session.PipelineResumeWindow)
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
	if err := envBoolOr("KNOMIT_RUNTIME_ALLOW_REMOTE", &cfg.Runtime.AllowRemote); err != nil {
		return Config{}, err
	}
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

	// Expand tildes in path fields. Home is not among them: ResolveHome
	// expanded it, once, before the knomit.toml search.
	for _, p := range []*string{
		&cfg.ONNXLibPath,
		&cfg.Remote.SSHKey,
		&cfg.Remote.KnownHosts,
		&cfg.LocalOriginRoot,
		&cfg.TLS.Dir,
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

	// Decide the local authenticated listener: KNOMIT_SOCKET, else the TOML
	// value decoded above, else the one this platform uses under <Home> --
	// defaulted for the same reason and at the same point as known_hosts,
	// after tilde expansion. socketFor is the one place those layers are
	// ordered; config.SocketPath (the bridge's side) reaches it too, so the
	// two cannot drift (knomit#271).
	//
	// It is the local bridge's credential -- the OS tells the server who is
	// on the other end (internal/auth.PeerCred) -- so it has to exist without
	// being configured. What guards it is the 0700 data root on unix and the
	// pipe ACL on Windows (internal/auth.ListenLocal opens both).
	//
	// EVERY platform gets one now. Windows had no default here through phase
	// 1, which is what made [auth].require = true a silent lockout there
	// (knomit#245): app.checkLocalListener refuses that combination, and the
	// pipe default is what lets Windows satisfy it.
	if cfg.Socket, err = socketFor(cfg.Home, cfg.Socket, os.Getenv("KNOMIT_SOCKET")); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	// Default [tls].dir to <Home>/pki, after tilde expansion like the two
	// above. The listener itself stays off until [tls].addr is set.
	if cfg.TLS.Dir == "" {
		cfg.TLS.Dir = filepath.Join(cfg.Home, "pki")
	}

	// One spelling of the issuer from here on: it is compared byte-for-byte
	// against every token's audience and every `iss` a client checks.
	if cfg.OAuth.Issuer != "" {
		norm, err := NormalizeIssuer(cfg.OAuth.Issuer)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		cfg.OAuth.Issuer = norm
	}
	if cfg.OAuth.IDP != nil {
		if err := cfg.OAuth.IDP.loadIDPSecret(); err != nil {
			return Config{}, err
		}
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
	// auth.loopback_hosts entries are exact DNS names. A port, a scheme, a
	// wildcard or a stray space would otherwise match nothing — or, for a
	// pattern, far more than the operator meant — so fail at boot.
	for _, h := range c.Auth.LoopbackHosts {
		if h == "" || strings.HasPrefix(h, ".") || strings.ContainsAny(h, ":/*@ \t") {
			return fmt.Errorf("config: auth.loopback_hosts entry %q is not a DNS name (exact names only: no port, scheme, wildcard or space)", h)
		}
	}
	// runtime.addr serves pprof and process controls with no authentication
	// (#288). A non-loopback bind — including ":6060", which binds every
	// interface — must be asked for, not stumbled into.
	if c.Runtime.Addr != "" {
		lo, err := addrIsLoopback(c.Runtime.Addr)
		if err != nil {
			return fmt.Errorf("config: runtime.addr %q is not host:port: %w", c.Runtime.Addr, err)
		}
		if !lo && !c.Runtime.AllowRemote {
			return fmt.Errorf("config: runtime.addr %q is not a loopback address; the runtime diagnostics port has no authentication. "+
				"Use 127.0.0.1:<port> or localhost:<port>, or set [runtime] allow_remote = true (KNOMIT_RUNTIME_ALLOW_REMOTE=true) to expose it deliberately", c.Runtime.Addr)
		}
	}
	return c.OAuth.validate()
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

// findConfigFile returns <homePath>/knomit.toml if it exists, else "".
//
// The data root is the ONLY place it looks. The server and the bridge are
// different executables, so a knomit.toml beside either binary would be read
// by that one alone and the two could resolve different local listeners. The
// desktop's Settings dialog writes <home>/knomit.toml too.
func findConfigFile(homePath string) string {
	p := filepath.Join(homePath, "knomit.toml")
	if fileExists(p) {
		return p
	}
	return ""
}

// ignoredExecutableConfig is the knomit.toml beside the running executable
// when one exists and is not also <homePath>/knomit.toml, else "". findConfigFile
// does not read it; Load warns about it once, so an install that kept its
// settings there learns where they must go.
func ignoredExecutableConfig(homePath string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "knomit.toml")
	if !fileExists(p) || filepath.Clean(p) == filepath.Join(homePath, "knomit.toml") {
		return ""
	}
	return p
}

// ignoredConfigWarnOnce limits Load's warning about an ignored knomit.toml
// beside the executable to one per process.
var ignoredConfigWarnOnce sync.Once

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

// expandTilde rewrites a leading "~/" (and, on Windows, `~\`) to the user's
// home directory.
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
	if !strings.HasPrefix(*s, "~/") &&
		!(filepath.Separator == '\\' && strings.HasPrefix(*s, `~\`)) {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot expand %q: %w", *s, err)
	}
	*s = filepath.Join(home, (*s)[2:])
	return nil
}
