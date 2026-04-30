package config

import (
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
