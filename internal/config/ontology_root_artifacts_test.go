package config

import (
	"strings"
	"testing"
)

// F25: artifacts/ is the agents' working-file root, kept out of discovery only
// by sitting BESIDE the ontology root. An ontology root that is that folder,
// or inside it, would make every fact an artifact and every artifact a fact,
// so the instance refuses to start, naming the setting.
func TestValidate_RejectsArtifactsOntologyRoot(t *testing.T) {
	for _, root := range []string{"artifacts", "Artifacts", "ARTIFACTS", "artifacts/", "/artifacts", "artifacts/kb", " artifacts "} {
		cfg := Defaults()
		cfg.OntologyRoot = root
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate() with ontology_root %q must return an error", root)
		}
		if !strings.Contains(err.Error(), "ontology_root") {
			t.Errorf("error %q should name ontology_root", err.Error())
		}
	}
	for _, root := range []string{"kb", "artifactsx", "kb/artifacts", "my-artifacts"} {
		cfg := Defaults()
		cfg.OntologyRoot = root
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with ontology_root %q: %v", root, err)
		}
	}
}

// The design's wording is "refuses to start": every production entry point
// (cmd/*, tools/desktop) gets its Config from Load, so the refusal must come
// out of Load for a knomit.toml that sets it — not only out of Validate.
func TestLoad_RefusesArtifactsOntologyRoot(t *testing.T) {
	writeConfig(t, "ontology_root = \"artifacts\"\n")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() must refuse ontology_root = \"artifacts\"")
	}
	if !strings.Contains(err.Error(), "ontology_root") {
		t.Errorf("error %q should name ontology_root", err.Error())
	}

	writeConfig(t, "ontology_root = \"kb\"\n")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with ontology_root = \"kb\": %v", err)
	}
}
