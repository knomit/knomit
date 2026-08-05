package main

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// manifest records exactly how a corpus was generated, so it can be
// regenerated close-enough (or at least have legible provenance) later, AND
// doubles as this build's resume checkpoint (see NextSlotStart/Complete
// below) — written after every batch, not just at the end, so a hard kill
// or crash (not just a clean error return) still leaves an accurate resume
// point. The .db files themselves are not committed anywhere; this file is
// the reproducible artifact.
type manifest struct {
	GeneratedAt        time.Time `json:"generated_at"`
	Size               int       `json:"size"`
	Diversity          string    `json:"diversity"`
	Ontology           string    `json:"ontology"`
	Topic              string    `json:"topic,omitempty"`
	ContentSource      string    `json:"content_source"`
	Seed               int64     `json:"seed"`
	LLMProvider        string    `json:"llm_provider"`
	LLMModel           string    `json:"llm_model"`
	EmbedModel         string    `json:"embed_model"`
	BatchSize          int       `json:"batch_size"`
	SharedRefsRate     float64   `json:"shared_refs_rate"`
	KeywordOverlapRate float64   `json:"keyword_overlap_rate"`
	Branch             string    `json:"branch"`
	FactCount          int       `json:"fact_count"`
	DBPath             string    `json:"db_path"`

	// Resume checkpoint fields.
	NextSlotStart int  `json:"next_slot_start"`
	DroppedTotal  int  `json:"dropped_total"`
	Complete      bool `json:"complete"`
}

// writeManifest and loadManifest take a full file path rather than a
// directory: --db mode points corpusgen at an arbitrary existing .db file
// (e.g. a copy of a live-daemon-registered repo), whose containing directory
// may be shared with other, unrelated repos (e.g. ~/.knomit/repos/) — a
// fixed "MANIFEST.json" name in that directory could collide with or read a
// different repo's manifest entirely. Callers derive the path: --out mode
// keeps the historical filepath.Join(out, "MANIFEST.json"); --db mode uses
// "<dbPath>.manifest.json", uniquely tied to that one file.
func writeManifest(path string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// loadManifest reads path, returning (nil, false, nil) if it doesn't exist
// yet (a genuinely fresh --out, or a foreign repo with no corpusgen
// manifest) rather than an error.
func loadManifest(path string) (*manifest, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, err
	}
	return &m, true, nil
}
