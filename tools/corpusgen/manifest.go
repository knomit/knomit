package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// manifest records exactly how a corpus was generated, so it can be
// regenerated close-enough (or at least have legible provenance) later. The
// .db files themselves are not committed anywhere; this file is the
// reproducible artifact.
type manifest struct {
	GeneratedAt        time.Time `json:"generated_at"`
	Size               int       `json:"size"`
	Diversity          string    `json:"diversity"`
	Ontology           string    `json:"ontology"`
	Topic              string    `json:"topic,omitempty"`
	Seed               int64     `json:"seed"`
	LLMProvider        string    `json:"llm_provider"`
	LLMModel           string    `json:"llm_model"`
	EmbedModel         string    `json:"embed_model"`
	BatchSize          int       `json:"batch_size"`
	SharedRefsRate     float64   `json:"shared_refs_rate"`
	KeywordOverlapRate float64   `json:"keyword_overlap_rate"`
	FactCount          int       `json:"fact_count"`
	DBPath             string    `json:"db_path"`
}

func writeManifest(outDir string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "MANIFEST.json"), data, 0o644)
}
