package config

import (
	"os"
	"strings"
	"testing"
)

// envBoolOr must accept every spelling strconv.ParseBool accepts, in both
// directions. Each case starts from the OPPOSITE default, so a helper that
// ignored the variable entirely would fail every row rather than half of them.
func TestEnvBoolOr_AcceptedSpellings(t *testing.T) {
	const key = "KNOMIT_TEST_BOOL"

	trueSpellings := []string{"1", "t", "T", "TRUE", "true", "True"}
	falseSpellings := []string{"0", "f", "F", "FALSE", "false", "False"}

	for _, v := range trueSpellings {
		t.Run("true/"+v, func(t *testing.T) {
			t.Setenv(key, v)
			target := false // opposite of the expected result
			if err := envBoolOr(key, &target); err != nil {
				t.Fatalf("envBoolOr(%q): unexpected error: %v", v, err)
			}
			if !target {
				t.Fatalf("envBoolOr(%q): want true, got false", v)
			}
		})
	}

	for _, v := range falseSpellings {
		t.Run("false/"+v, func(t *testing.T) {
			t.Setenv(key, v)
			target := true // opposite of the expected result
			if err := envBoolOr(key, &target); err != nil {
				t.Fatalf("envBoolOr(%q): unexpected error: %v", v, err)
			}
			if target {
				t.Fatalf("envBoolOr(%q): want false, got true", v)
			}
		})
	}
}

// Surrounding whitespace is an artifact of how the value was exported, not a
// different value: " true " means true, and a whitespace-only value is as good
// as unset.
func TestEnvBoolOr_TrimsWhitespace(t *testing.T) {
	const key = "KNOMIT_TEST_BOOL"

	t.Run("padded true", func(t *testing.T) {
		t.Setenv(key, "  true\t")
		target := false
		if err := envBoolOr(key, &target); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !target {
			t.Fatal(`envBoolOr("  true\t"): want true, got false`)
		}
	})

	t.Run("padded false", func(t *testing.T) {
		t.Setenv(key, " 0 ")
		target := true
		if err := envBoolOr(key, &target); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if target {
			t.Fatal(`envBoolOr(" 0 "): want false, got true`)
		}
	})

	t.Run("whitespace only keeps default", func(t *testing.T) {
		t.Setenv(key, "   ")
		target := true
		if err := envBoolOr(key, &target); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !target {
			t.Fatal("whitespace-only value must leave the default untouched")
		}
	})
}

// Unset and empty both mean "no override" — the default survives in either
// direction, so this is checked against both a true and a false default.
func TestEnvBoolOr_EmptyKeepsDefault(t *testing.T) {
	const key = "KNOMIT_TEST_BOOL"

	for _, def := range []bool{true, false} {
		t.Run("empty", func(t *testing.T) {
			t.Setenv(key, "")
			target := def
			if err := envBoolOr(key, &target); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if target != def {
				t.Fatalf("empty value must leave default %v untouched; got %v", def, target)
			}
		})
		t.Run("unset", func(t *testing.T) {
			t.Setenv(key, "x") // registers the cleanup that restores the prior state
			if err := os.Unsetenv(key); err != nil {
				t.Fatalf("Unsetenv: %v", err)
			}
			target := def
			if err := envBoolOr(key, &target); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if target != def {
				t.Fatalf("unset variable must leave default %v untouched; got %v", def, target)
			}
		})
	}
}

// A value that is neither a ParseBool spelling nor empty is an operator
// mistake, not a false: erroring is what keeps KNOMIT_GIT_SERVE=yes from
// silently disabling git serving. The error must name the variable so the
// boot failure points at what to fix.
func TestEnvBoolOr_RejectsOtherSpellings(t *testing.T) {
	const key = "KNOMIT_TEST_BOOL"

	for _, v := range []string{"yes", "no", "on", "off", "2", "junk", "-1", "true false"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(key, v)
			target := true
			err := envBoolOr(key, &target)
			if err == nil {
				t.Fatalf("envBoolOr(%q): want an error, got nil", v)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error must name the variable %q; got %q", key, err.Error())
			}
			if !strings.Contains(err.Error(), v) {
				t.Errorf("error must quote the offending value %q; got %q", v, err.Error())
			}
			if !target {
				t.Error("a rejected value must leave the default untouched")
			}
		})
	}
}

// The regression that took /git down during acceptance of PR #205:
// KNOMIT_GIT_SERVE=1 read as "not the literal string true" and turned OFF a
// feature that defaults to on.
func TestLoad_GitServeOneIsTrue(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir()) // empty dir → no TOML, defaults + env only
	t.Setenv("KNOMIT_GIT_SERVE", "1")

	if !Defaults().Git.Serve {
		t.Fatal("precondition: Git.Serve must default to true for this regression to bite")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Git.Serve {
		t.Fatal("KNOMIT_GIT_SERVE=1 must enable git serving, not disable it")
	}
}

// KNOMIT_GIT_SERVE=0 must still turn serving off — the fix must not make the
// variable write-once-true.
func TestLoad_GitServeZeroIsFalse(t *testing.T) {
	t.Setenv("KNOMIT_HOME", t.TempDir())
	t.Setenv("KNOMIT_GIT_SERVE", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Git.Serve {
		t.Fatal("KNOMIT_GIT_SERVE=0 must disable git serving")
	}
}

// Load propagates the rejection: boot fails loudly instead of applying a
// value nobody chose.
func TestLoad_MalformedBoolEnvErrors(t *testing.T) {
	for _, key := range []string{"KNOMIT_LLM_CACHE", "KNOMIT_LLM_BATCH", "KNOMIT_GIT_SERVE", "KNOMIT_READ_ONLY"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("KNOMIT_HOME", t.TempDir())
			t.Setenv(key, "yes")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load with %s=yes must fail, not silently apply false", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("Load error must name %q; got %q", key, err.Error())
			}
		})
	}
}
