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
		envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v)
		if v != 0.42 {
			t.Fatalf("want 0.42, got %v", v)
		}
	})
	t.Run("unparseable keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_METHODOLOGY_MIN_SCORE", "not-a-number")
		v := 0.15
		envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v)
		if v != 0.15 {
			t.Fatalf("unparseable value must leave default untouched; got %v", v)
		}
	})
	t.Run("empty keeps default", func(t *testing.T) {
		t.Setenv("KNOMIT_METHODOLOGY_MIN_SCORE", "")
		v := 0.15
		envFloatOr("KNOMIT_METHODOLOGY_MIN_SCORE", &v)
		if v != 0.15 {
			t.Fatalf("empty value must leave default untouched; got %v", v)
		}
	})
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
