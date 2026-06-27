package config

import (
	"math"
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
		t.Setenv("KNOMIT_CLUSTER_CACHE_MAX_CONCURRENT", "4")
		v := 1
		if err := envIntOr("KNOMIT_CLUSTER_CACHE_MAX_CONCURRENT", &v); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != 4 {
			t.Fatalf("want 4, got %v", v)
		}
	})
	t.Run("unparseable errors and keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_CLUSTER_CACHE_MAX_CONCURRENT", "lots")
		v := 1
		if err := envIntOr("KNOMIT_CLUSTER_CACHE_MAX_CONCURRENT", &v); err == nil {
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
// default at 2.0 (was a hardcoded 1.0): higher γ breaks the over-large
// communities surfaced by the search-clustering analysis (mega-cluster 65→27).
func TestDefaults_ClusterResolution(t *testing.T) {
	d := Defaults()
	if got := d.ClusterCache.Resolution; got != 2.0 {
		t.Fatalf("Defaults().ClusterCache.Resolution: want 2.0, got %v", got)
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
