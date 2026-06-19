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
