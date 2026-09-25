package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidate_RejectsEmptyOntologyRoot regresses the bug where a TOML
// override of ontology_root="" let synthesize sessions complete
// "successfully" while every distill/prune output was silently rejected
// at validateOutputPath. Fail-fast at config load instead.
func TestValidate_RejectsEmptyOntologyRoot(t *testing.T) {
	cfg := Defaults()
	cfg.OntologyRoot = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with empty OntologyRoot must return an error")
	}
	if !strings.Contains(err.Error(), "ontology_root") {
		t.Errorf("error %q should mention ontology_root", err.Error())
	}
}

// TestValidate_RejectsWhitespaceOntologyRoot guards against an override
// like ontology_root = " " — strings.TrimSpace on the way in keeps a
// later validateOutputPath from building a prefix of " /".
func TestValidate_RejectsWhitespaceOntologyRoot(t *testing.T) {
	cfg := Defaults()
	cfg.OntologyRoot = "  "
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() with whitespace-only OntologyRoot must return an error")
	}
}

func TestValidate_DefaultsAreValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("Defaults() must validate cleanly, got: %v", err)
	}
}

// TestDefaults_MethodologyMinScore pins the documented default. The value
// is load-bearing for prompt injection — a regression to 0 admits every
// candidate; a regression to 1 admits none.
func TestDefaults_MethodologyMinScore(t *testing.T) {
	if got := Defaults().MethodologyMinScore; got != 0.15 {
		t.Fatalf("Defaults().MethodologyMinScore: want 0.15, got %v", got)
	}
}

// TestDefaults_DiscoveryConfig pins the design-spec defaults. The
// effort_default=normal guarantee is the byte-identical-pre-discovery
// regression contract.
func TestDefaults_DiscoveryConfig(t *testing.T) {
	d := Defaults().Discovery
	if d.EffortDefault != "normal" {
		t.Errorf("Discovery.EffortDefault: want normal, got %q", d.EffortDefault)
	}
	if d.ConfidenceThreshold != 0.5 {
		t.Errorf("Discovery.ConfidenceThreshold: want 0.5, got %v", d.ConfidenceThreshold)
	}
	if d.BlastRadiusThreshold != 1 {
		t.Errorf("Discovery.BlastRadiusThreshold: want 1, got %d", d.BlastRadiusThreshold)
	}
	if d.Bridge != "both" {
		t.Errorf("Discovery.Bridge: want both, got %q", d.Bridge)
	}
}

// TestValidate_MethodologyMinScore_RejectsOutOfRange guards against
// silent misbehavior when a user sets the threshold outside [0, 1].
// Negative or >1 values either admit everything or filter everything
// without a log line; NaN silently disables the comparison entirely.
func TestValidate_MethodologyMinScore_RejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		v    float64
	}{
		{"negative", -0.01},
		{"above one", 1.01},
		{"NaN", math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.MethodologyMinScore = tc.v
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() with MethodologyMinScore=%v must error", tc.v)
			}
			if !strings.Contains(err.Error(), "methodology_min_score") {
				t.Errorf("error %q should mention methodology_min_score", err.Error())
			}
		})
	}
}

// TestValidate_DiscoveryEffortDefault_RejectsUnknown guards against a typo'd
// discovery.effort_default passing boot and then failing EVERY no-argument
// review/hypothesize call at runtime with a confusing "invalid effort" error.
// Unknown values must fail at boot; "" and the three valid efforts must pass.
func TestValidate_DiscoveryEffortDefault_RejectsUnknown(t *testing.T) {
	for _, bad := range []string{"turbo", "medum", "high ", "Normal", "0"} {
		cfg := Defaults()
		cfg.Discovery.EffortDefault = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() must reject discovery.effort_default=%q", bad)
		}
	}
	for _, ok := range []string{"", "normal", "medium", "high"} {
		cfg := Defaults()
		cfg.Discovery.EffortDefault = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() must accept discovery.effort_default=%q, got %v", ok, err)
		}
	}
}

// TestValidate_DiscoveryBridge_RejectsUnknown guards the sibling string knob.
// Even though it is coerced downstream, a typo must fail loudly at boot rather
// than silently widening the bridge axis to "both".
func TestValidate_DiscoveryBridge_RejectsUnknown(t *testing.T) {
	for _, bad := range []string{"entties", "Both", "all", "entity "} {
		cfg := Defaults()
		cfg.Discovery.Bridge = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() must reject discovery.bridge=%q", bad)
		}
	}
	for _, ok := range []string{"", "domain", "entity", "both"} {
		cfg := Defaults()
		cfg.Discovery.Bridge = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() must accept discovery.bridge=%q, got %v", ok, err)
		}
	}
}

// TestValidate_DiscoveryConfidenceThreshold_RejectsOutOfRange is the regression
// guard for the missing range check on discovery.confidence_threshold. Before the
// fix, negative or >1 values passed Validate(), reached
// RepoInstance.DiscoveryConfidenceThreshold(), and silently filtered all
// proposals (>1) or disabled the gate without operator intent (negative).
func TestValidate_DiscoveryConfidenceThreshold_RejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		v    float64
	}{
		{"negative", -0.01},
		{"above one", 1.01},
		{"NaN", math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Discovery.ConfidenceThreshold = tc.v
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() with ConfidenceThreshold=%v must error", tc.v)
			}
			if !strings.Contains(err.Error(), "confidence_threshold") {
				t.Errorf("error %q should mention confidence_threshold", err.Error())
			}
		})
	}
}

// TestValidate_DiscoveryConfidenceThreshold_AcceptsZero ensures 0 passes
// Validate — it is the documented "disable the gate" value and must not be
// treated as missing/invalid.
func TestValidate_DiscoveryConfidenceThreshold_AcceptsZero(t *testing.T) {
	cfg := Defaults()
	cfg.Discovery.ConfidenceThreshold = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() must accept ConfidenceThreshold=0 (gate-disabled value), got: %v", err)
	}
}

// TestValidate_DiscoveryBlastRadiusThreshold_RejectsNegative guards that a
// typo'd negative blast_radius_threshold fails at boot rather than silently
// disabling the keystone gate (negative behaves like the documented 0-disable
// but carries no intent). 0 itself remains valid (gate-disabled value).
func TestValidate_DiscoveryBlastRadiusThreshold_RejectsNegative(t *testing.T) {
	cfg := Defaults()
	cfg.Discovery.BlastRadiusThreshold = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with BlastRadiusThreshold=-1 must error")
	}
	if !strings.Contains(err.Error(), "blast_radius_threshold") {
		t.Errorf("error %q should mention blast_radius_threshold", err.Error())
	}

	cfg.Discovery.BlastRadiusThreshold = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() must accept BlastRadiusThreshold=0 (gate-disabled value), got: %v", err)
	}
}

// TestLoad_DiscoveryEnvOverrides verifies all four discovery config knobs wire
// through from KNOMIT_DISCOVERY_* env vars to the loaded config (parity with
// TestLoad_ClusterResolutionEnvOverride).
func TestLoad_DiscoveryEnvOverrides(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_DISCOVERY_EFFORT_DEFAULT", "high")
	t.Setenv("KNOMIT_DISCOVERY_BRIDGE", "entity")
	t.Setenv("KNOMIT_DISCOVERY_CONFIDENCE_THRESHOLD", "0.8")
	t.Setenv("KNOMIT_DISCOVERY_BLAST_RADIUS_THRESHOLD", "10")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Discovery.EffortDefault; got != "high" {
		t.Errorf("env override EffortDefault: want high, got %q", got)
	}
	if got := cfg.Discovery.Bridge; got != "entity" {
		t.Errorf("env override Bridge: want entity, got %q", got)
	}
	if got := cfg.Discovery.ConfidenceThreshold; got != 0.8 {
		t.Errorf("env override ConfidenceThreshold: want 0.8, got %v", got)
	}
	if got := cfg.Discovery.BlastRadiusThreshold; got != 10 {
		t.Errorf("env override BlastRadiusThreshold: want 10, got %d", got)
	}
}

// TestValidate_MethodologyMinScore_AcceptsBoundsAndZero covers the
// in-range edges so the validator does not over-reject.
func TestValidate_MethodologyMinScore_AcceptsBoundsAndZero(t *testing.T) {
	for _, v := range []float64{0, 0.15, 0.5, 1.0} {
		cfg := Defaults()
		cfg.MethodologyMinScore = v
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() with MethodologyMinScore=%v must accept, got %v", v, err)
		}
	}
}

// TestEnvFloatOr_MethodologyMinScore exercises the env-var override
// path. Bad values are silently ignored (default kept); good values
// override the default.
func TestEnvFloatOr_MethodologyMinScore(t *testing.T) {
	t.Run("valid value overrides", func(t *testing.T) {
		t.Setenv("KNOMIT_METHODOLOGY_MIN_SCORE", "0.42")
		v := 0.15
		if err := envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 0.42 {
			t.Fatalf("want 0.42, got %v", v)
		}
	})
	t.Run("unparseable errors and keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_METHODOLOGY_MIN_SCORE", "not-a-number")
		v := 0.15
		if err := envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v); err == nil {
			t.Fatal("malformed value must error, not be silently ignored")
		}
		if v != 0.15 {
			t.Fatalf("malformed value must leave default untouched; got %v", v)
		}
	})
	t.Run("empty keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_METHODOLOGY_MIN_SCORE", "")
		v := 0.15
		if err := envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 0.15 {
			t.Fatalf("empty value must leave default untouched; got %v", v)
		}
	})
}

func TestEnvIntOr(t *testing.T) {
	t.Run("valid value overrides", func(t *testing.T) {
		t.Setenv("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", "4")
		v := 1
		if err := envIntOr("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 4 {
			t.Fatalf("want 4, got %v", v)
		}
	})
	t.Run("unparseable errors and keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", "lots")
		v := 1
		if err := envIntOr("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", &v); err == nil {
			t.Fatal("malformed value must error, not be silently ignored")
		}
		if v != 1 {
			t.Fatalf("malformed value must leave default untouched; got %v", v)
		}
	})
}

// TestLoad_MalformedNumericEnvErrors regresses the gap where a set-but-malformed
// numeric env override (e.g. KNOMIT_CLUSTER_CACHE_RESOLUTION="two") was silently
// dropped, leaving the default in place with no signal. Load must now surface it
// at boot.
func TestLoad_MalformedNumericEnvErrors(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_CLUSTER_CACHE_RESOLUTION", "two")

	if _, err := Load(); err == nil {
		t.Fatal("Load must reject a malformed numeric env var, got nil error")
	}
}

// TestDefaults_ClusterResolution pins the configurable Louvain resolution
// default at 4.0: calibrated for the SIMILAR_TO-only review subgraph clustered
// by gonum so it yields review-sized communities matching the prior
// global-Louvain granularity (~35 communities vs the coarse ~17 at γ=2.0).
func TestDefaults_ClusterResolution(t *testing.T) {
	d := Defaults()
	if got := d.ClusterCache.Resolution; got != 4.0 {
		t.Fatalf("Defaults().ClusterCache.Resolution: want 4.0, got %v", got)
	}
	if got := d.ClusterCache.MinCommunitySize; got != 2 {
		t.Fatalf("Defaults().ClusterCache.MinCommunitySize: want 2, got %v", got)
	}
}

func TestEmbeddingsModelDefault(t *testing.T) {
	c := Defaults()
	if c.Embeddings.Model != "embeddinggemma" {
		t.Errorf("default embeddings model = %q, want embeddinggemma", c.Embeddings.Model)
	}
}

func TestEmbeddingsModelEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // isolate from any real ~/.knomit/knomit.toml
	t.Setenv("KNOMIT_EMBED_MODEL", "nomic-v1.5")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Embeddings.Model != "nomic-v1.5" {
		t.Errorf("env override = %q, want nomic-v1.5", c.Embeddings.Model)
	}
}

// TestLoad_ClusterResolutionEnvOverride regresses the gap where the new
// cluster_cache resolution / min_community_size fields had no env override
// (the other three fields did). Load must wire KNOMIT_CLUSTER_CACHE_RESOLUTION
// and KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE through to the config.
func TestLoad_ClusterResolutionEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_CLUSTER_CACHE_RESOLUTION", "1.5")
	t.Setenv("KNOMIT_CLUSTER_CACHE_MIN_COMMUNITY_SIZE", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.ClusterCache.Resolution; got != 1.5 {
		t.Fatalf("env override Resolution: want 1.5, got %v", got)
	}
	if got := cfg.ClusterCache.MinCommunitySize; got != 3 {
		t.Fatalf("env override MinCommunitySize: want 3, got %v", got)
	}
}

// TestDefaults_DiscoveryQKnobs pins the six new quality-scorer config defaults
// added in Task 7. These values are the seeds for the calibrate tool; a
// regression to zero or a wrong weight silently mis-scores every bridge set.
func TestDefaults_DiscoveryQKnobs(t *testing.T) {
	d := Defaults().Discovery
	if d.CohFloor != 0.5 {
		t.Errorf("Discovery.CohFloor: want 0.5, got %v", d.CohFloor)
	}
	if d.MaxMembers != 5 {
		t.Errorf("Discovery.MaxMembers: want 5, got %d", d.MaxMembers)
	}
	if d.QualityFloor != 0.0 {
		t.Errorf("Discovery.QualityFloor: want 0.0, got %v", d.QualityFloor)
	}
	if d.WCoh != 1.0 {
		t.Errorf("Discovery.WCoh: want 1.0, got %v", d.WCoh)
	}
	if d.WGap != 1.0 {
		t.Errorf("Discovery.WGap: want 1.0, got %v", d.WGap)
	}
	if d.WSpec != 1.0 {
		t.Errorf("Discovery.WSpec: want 1.0, got %v", d.WSpec)
	}
}

// TestLoad_LocalOriginRootEnvOverride verifies KNOMIT_LOCAL_ORIGIN_ROOT wires
// through to cfg.LocalOriginRoot (the gate for local-path git origins). The
// default is empty, which disables local origins.
func TestLoad_LocalOriginRootEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LocalOriginRoot != "" {
		t.Fatalf("default LocalOriginRoot: want empty, got %q", cfg.LocalOriginRoot)
	}

	t.Setenv("KNOMIT_LOCAL_ORIGIN_ROOT", "/srv/kb")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LocalOriginRoot; got != "/srv/kb" {
		t.Fatalf("env override LocalOriginRoot: want /srv/kb, got %q", got)
	}
}

func TestReadOnly_DefaultsFalse(t *testing.T) {
	if Defaults().ReadOnly {
		t.Fatal("ReadOnly should default to false")
	}
}

func TestReadOnly_EnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_READ_ONLY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ReadOnly {
		t.Fatal("KNOMIT_READ_ONLY=true should set cfg.ReadOnly")
	}
}

// TestDefaults_MaxBatchTokensIsAutoSentinel pins the sentinel. It is
// deliberately ABSENT from Defaults(): Defaults() runs before the TOML and env
// layers, so a value written there could not be distinguished from an operator
// who set the same number explicitly. The zero value is what lets the app layer
// tell "unset" from "chosen", the way remote.known_hosts already does.
func TestDefaults_MaxBatchTokensIsAutoSentinel(t *testing.T) {
	if got := Defaults().Embeddings.MaxBatchTokens; got != 0 {
		t.Errorf("Defaults().Embeddings.MaxBatchTokens = %d, want 0 (the auto sentinel)", got)
	}
}

// TestValidate_MaxBatchTokens_AcceptsZero guards the sentinel against a future
// tightening to "must be > 0", which would reject every config that leaves the
// knob alone.
func TestValidate_MaxBatchTokens_AcceptsZero(t *testing.T) {
	cfg := Defaults()
	cfg.Embeddings.MaxBatchTokens = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected the auto sentinel: %v", err)
	}
}

// TestValidate_MaxBatchTokens_RejectsNegative fails a meaningless budget at
// boot. A negative value reaching the packer degrades it to one document per
// inference — a silent ~30x re-embed slowdown rather than a visible error.
func TestValidate_MaxBatchTokens_RejectsNegative(t *testing.T) {
	cfg := Defaults()
	cfg.Embeddings.MaxBatchTokens = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a negative max_batch_tokens")
	}
	if !strings.Contains(err.Error(), "max_batch_tokens") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestLoad_MaxBatchTokensEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_EMBED_MAX_BATCH_TOKENS", "8192")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Embeddings.MaxBatchTokens; got != 8192 {
		t.Errorf("env override MaxBatchTokens: want 8192, got %d", got)
	}
}

func TestDefaults_ClientSessionThresholds(t *testing.T) {
	cfg := Defaults()
	if cfg.Session.ClientDeadAfter != "1h" {
		t.Errorf("ClientDeadAfter = %q, want 1h", cfg.Session.ClientDeadAfter)
	}
	if cfg.Session.ClientHiddenAfter != "3h" {
		t.Errorf("ClientHiddenAfter = %q, want 3h", cfg.Session.ClientHiddenAfter)
	}
	if cfg.Session.ClientRetention != "168h" {
		t.Errorf("ClientRetention = %q, want 168h", cfg.Session.ClientRetention)
	}
}

// Empty on purpose: the default is applied where it is validated against
// pipeline_idle_ttl, so a config that only shortens that TTL still boots.
func TestDefaults_PipelineResumeWindowIsUnset(t *testing.T) {
	if got := Defaults().Session.PipelineResumeWindow; got != "" {
		t.Errorf("PipelineResumeWindow = %q, want empty", got)
	}
}

func TestLoad_PipelineResumeWindowEnvOverride(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_SESSION_PIPELINE_RESUME_WINDOW", "4m")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.PipelineResumeWindow != "4m" {
		t.Errorf("PipelineResumeWindow = %q, want 4m", cfg.Session.PipelineResumeWindow)
	}
}

func TestLoad_ClientSessionEnvOverrides(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_SESSION_CLIENT_DEAD_AFTER", "30m")
	t.Setenv("KNOMIT_SESSION_CLIENT_HIDDEN_AFTER", "2h")
	t.Setenv("KNOMIT_SESSION_CLIENT_RETENTION", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.ClientDeadAfter != "30m" {
		t.Errorf("ClientDeadAfter = %q, want 30m", cfg.Session.ClientDeadAfter)
	}
	if cfg.Session.ClientHiddenAfter != "2h" {
		t.Errorf("ClientHiddenAfter = %q, want 2h", cfg.Session.ClientHiddenAfter)
	}
	if cfg.Session.ClientRetention != "0" {
		t.Errorf("ClientRetention = %q, want 0", cfg.Session.ClientRetention)
	}
}

// F19 phase 1: [auth] exists and its defaults keep an upgrade uneventful —
// require off, and the anonymous loopback caller holds everything a local
// operator held before authentication existed.
func TestDefaults_AuthLoopbackIsFullLocalRights(t *testing.T) {
	d := Defaults()
	if d.Auth.Require {
		t.Fatal("require must default false so an upgrade breaks nobody")
	}
	want := []string{"read", "write", "push:own", "operator", "admin"}
	if strings.Join(d.Auth.LoopbackDefault, ",") != strings.Join(want, ",") {
		t.Fatalf("loopback_default = %v, want %v", d.Auth.LoopbackDefault, want)
	}
}

// The local listener is the local credential, so it must exist without being
// asked for: Load fills it in from Home when nothing set it. EVERY platform,
// since knomit#245 -- Windows having no default here is what made
// [auth].require = true a silent lockout there.
func TestLoad_SocketDefaultsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket == "" {
		t.Fatal("Load left Socket empty; with [auth].require = true that is a boot failure by design")
	}
	if want := localListenerName(home); cfg.Socket != want {
		t.Fatalf("Socket = %q, want %q", cfg.Socket, want)
	}
}

// An explicit socket path must survive: the default only fills a gap.
func TestLoad_ExplicitSocketWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	// Not a valid listener path on either platform, which is the point: Load
	// must not second-guess an operator, and app.checkLocalListener only asks
	// whether one is configured. auth.ListenLocal is what refuses a bad one,
	// at boot, loudly.
	t.Setenv("KNOMIT_SOCKET", "/tmp/explicit.sock")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/tmp/explicit.sock" {
		t.Fatalf("Socket = %q, want the explicit path", cfg.Socket)
	}
}

// The bridge dials the socket the server opens. They resolve it through the
// same helper precisely so they cannot drift; this pins that they agree.
func TestSocketPath_AgreesWithTheResolvedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg.Socket {
		t.Fatalf("SocketPath() = %q but the server listens on %q — a bridge computing this itself would silently fall back to TCP", got, cfg.Socket)
	}
}

// KNOMIT_REPO is the backward-compatible alias for the data root, and the
// socket has to follow it too.
func TestSocketPath_HonoursTheRepoAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", "")
	t.Setenv("KNOMIT_REPO", home)
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := localListenerName(home); got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
}

// The nil fallback has exactly one definition, because web.Server.grants()
// and app.seedOwnPrincipal both read it: if they ever disagreed, boot would seed
// the server's own local principal with one permission set while the middleware resolved
// anonymous to another.
func TestEffectiveLoopbackDefault_ThreeCases(t *testing.T) {
	// nil — the literal was built without config, which only tests do.
	if got := (AuthConfig{}).EffectiveLoopbackDefault(); strings.Join(got, ",") !=
		strings.Join(Defaults().Auth.LoopbackDefault, ",") {
		t.Fatalf("nil must fall back to the defaults, got %v", got)
	}

	// EMPTY BUT NON-NIL — `loopback_default = []` in TOML. An operator who
	// writes an empty list means it; falling back here would silently grant
	// the full default set to someone who asked for none of it.
	got := AuthConfig{LoopbackDefault: []string{}}.EffectiveLoopbackDefault()
	if len(got) != 0 {
		t.Fatalf("an explicitly empty list must stay empty, got %v", got)
	}

	// Populated — returned as written.
	want := []string{"read"}
	if got := (AuthConfig{LoopbackDefault: want}).EffectiveLoopbackDefault(); strings.Join(got, ",") != "read" {
		t.Fatalf("a populated list must be returned as written, got %v", got)
	}
}

// [tls] is OFF by default (no Addr) and its Dir follows Home, so enrolling
// needs no config beyond turning the listener on.
func TestLoad_TLSDefaultsOffWithDirUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.Addr != "" {
		t.Fatalf("TLS.Addr must default empty (listener off), got %q", cfg.TLS.Addr)
	}
	if want := filepath.Join(home, "pki"); cfg.TLS.Dir != want {
		t.Fatalf("TLS.Dir = %q, want %q", cfg.TLS.Dir, want)
	}
	t.Setenv("KNOMIT_TLS_ADDR", "0.0.0.0:19279")
	t.Setenv("KNOMIT_TLS_DIR", "/srv/knomit/pki")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.Addr != "0.0.0.0:19279" || cfg.TLS.Dir != "/srv/knomit/pki" {
		t.Fatalf("env overrides: %+v", cfg.TLS)
	}
}

// [auth].loopback_hosts (#281): the extra DNS names a loopback peer may use
// and still be the anonymous principal. EffectiveLoopbackHosts adds the BIND
// host when it is a DNS name, and lower-cases everything; it is evaluated
// where the Server is built, from the POST-override cfg.Host (knomit serve
// --host is applied after Load), never at load.
func TestEffectiveLoopbackHosts(t *testing.T) {
	a := AuthConfig{LoopbackHosts: []string{"Box.Tail1234.TS.net", "dev.example"}}
	for _, c := range []struct {
		bind string
		want string
	}{
		{"mybox", "box.tail1234.ts.net,dev.example,mybox"},
		{"MyBox.lan", "box.tail1234.ts.net,dev.example,mybox.lan"},
		{"localhost", "box.tail1234.ts.net,dev.example,localhost"},
		// Wildcards and IP literals name no host: IP literals pass the guard
		// anyway, and a wildcard is not a name anyone browses to.
		{"", "box.tail1234.ts.net,dev.example"},
		{"0.0.0.0", "box.tail1234.ts.net,dev.example"},
		{"::", "box.tail1234.ts.net,dev.example"},
		{"[::1]", "box.tail1234.ts.net,dev.example"},
		{"127.0.0.1", "box.tail1234.ts.net,dev.example"},
	} {
		if got := strings.Join(a.EffectiveLoopbackHosts(c.bind), ","); got != c.want {
			t.Errorf("bind %q: got %q, want %q", c.bind, got, c.want)
		}
	}
	if got := (AuthConfig{}).EffectiveLoopbackHosts(""); len(got) != 0 {
		t.Fatalf("nothing configured, no bind name: got %v", got)
	}
}

// A loopback_hosts entry is an exact DNS name. Anything that looks like a
// port, a URL, a pattern or a typo fails the boot rather than silently
// matching nothing (or, for a pattern, too much).
func TestValidate_LoopbackHostsRefusesNonNames(t *testing.T) {
	for _, bad := range []string{"", " ", "box:443", "https://box", "box/", "*.ts.net", "*", "my box", ".ts.net"} {
		c := Defaults()
		c.Auth.LoopbackHosts = []string{"ok.example", bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "loopback_hosts") {
			t.Errorf("entry %q: want a loopback_hosts error, got %v", bad, err)
		}
	}
	c := Defaults()
	c.Auth.LoopbackHosts = []string{"box.tail1234.ts.net", "MyBox"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid names refused: %v", err)
	}
}

func TestLoad_LoopbackHostsFromTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"),
		[]byte("[auth]\nloopback_hosts = [\"Box.Tail1234.ts.net\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(cfg.Auth.EffectiveLoopbackHosts(cfg.Host), ","); got != "box.tail1234.ts.net,localhost" {
		t.Fatalf("got %q", got)
	}
}

// runtime.addr serves pprof and process controls unauthenticated (#288): a
// bind that is not loopback — ":6060" included, which binds every interface —
// fails the boot unless runtime.allow_remote asks for it.
func TestValidate_RuntimeAddrMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6060", "127.0.0.2:6060", "[::1]:6060", "localhost:6060", "LocalHost:6060"} {
		c := Defaults()
		c.Runtime.Addr = addr
		if err := c.Validate(); err != nil {
			t.Errorf("loopback %q refused: %v", addr, err)
		}
	}
	for _, addr := range []string{":6060", "0.0.0.0:6060", "[::]:6060", "192.168.1.5:6060", "knomit.example:6060", "localhost.:6060"} {
		c := Defaults()
		c.Runtime.Addr = addr
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "allow_remote") {
			t.Errorf("non-loopback %q: want an error naming allow_remote, got %v", addr, err)
		}
		c.Runtime.AllowRemote = true
		if err := c.Validate(); err != nil {
			t.Errorf("non-loopback %q with allow_remote refused: %v", addr, err)
		}
	}
	for _, addr := range []string{"6060", "127.0.0.1"} {
		c := Defaults()
		c.Runtime.Addr = addr
		c.Runtime.AllowRemote = true
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "host:port") {
			t.Errorf("malformed %q: want a host:port error, got %v", addr, err)
		}
	}
}

func TestLoad_RuntimeAllowRemoteFromEnv(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_RUNTIME_ADDR", ":6060")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "KNOMIT_RUNTIME_ALLOW_REMOTE") {
		t.Fatalf("Load with :6060 and no opt-in: want an error naming the env var, got %v", err)
	}
	t.Setenv("KNOMIT_RUNTIME_ALLOW_REMOTE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with opt-in: %v", err)
	}
	if !cfg.Runtime.AllowRemote {
		t.Fatal("KNOMIT_RUNTIME_ALLOW_REMOTE=true did not set Runtime.AllowRemote")
	}
	t.Setenv("KNOMIT_RUNTIME_ALLOW_REMOTE", "yes-please")
	if _, err := Load(); err == nil {
		t.Fatal("a non-bool KNOMIT_RUNTIME_ALLOW_REMOTE was accepted")
	}
}

func TestLoad_RuntimeAllowRemoteFromTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KNOMIT_HOME", home)
	toml := "[runtime]\naddr = \"0.0.0.0:6060\"\nallow_remote = true\n"
	if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Runtime.AllowRemote || cfg.Runtime.Addr != "0.0.0.0:6060" {
		t.Fatalf("runtime = %+v", cfg.Runtime)
	}
}

// knomit.toml is read from the data root and nowhere else. The server and the
// bridge are different executables, so a file beside either one would be seen
// by that binary alone and the two could resolve different sockets. The file
// is planted beside THIS test binary, the one os.Executable names.
func TestFindConfigFile_OnlyHome(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	beside := filepath.Join(filepath.Dir(exe), "knomit.toml")
	if _, err := os.Stat(beside); err == nil {
		t.Skipf("%s already exists; not overwriting it", beside)
	}
	if err := os.WriteFile(beside, []byte("socket = '/beside/exe.sock'\n"), 0o600); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	home := t.TempDir()
	if got := findConfigFile(home); got != "" {
		t.Fatalf("findConfigFile(%q) = %q with no <home>/knomit.toml; want \"\"", home, got)
	}
	inHome := filepath.Join(home, "knomit.toml")
	if err := os.WriteFile(inHome, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findConfigFile(home); got != inHome {
		t.Fatalf("findConfigFile(%q) = %q, want %q", home, got, inHome)
	}
}
