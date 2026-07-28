package repos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// adoptFromFilesystem performs the ONE-TIME migration from filesystem-as-registry
// to control.db-as-registry. It runs only when the registry is empty; a populated
// registry is authoritative and is never reconciled against the disk again.
//
// Returns the number of rows adopted.
func adoptFromFilesystem(reg *RepoRegistry, reposDir string) (int, error) {
	existing, err := reg.List("")
	if err != nil {
		return 0, fmt.Errorf("adopt: read registry: %w", err)
	}
	if len(existing) > 0 {
		return 0, nil
	}

	adopted := 0

	dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
	sort.Strings(dbFiles)
	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		if store.IsSessionDBFile(base) {
			continue
		}
		name := strings.TrimSuffix(base, ".db")
		if !isValidRepoName(name) {
			log.Warn().Str("file", base).Msg("adopt: skipping db with invalid repo name")
			continue
		}
		if err := reg.Upsert(RepoRecord{Name: name, State: RepoActive}); err != nil {
			return adopted, err
		}
		adopted++
	}

	// Archived repos carry their metadata in legacy JSON manifests. Fold them in
	// so archive state survives the switch — and so the JSON can stop existing.
	manifests, _ := filepath.Glob(filepath.Join(reposDir, "archive", "*.json"))
	sort.Strings(manifests)
	for _, path := range manifests {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			log.Warn().Err(rerr).Str("file", path).Msg("adopt: unreadable archive manifest, skipping")
			continue
		}
		var info ArchiveInfo
		if err := json.Unmarshal(data, &info); err != nil {
			log.Warn().Err(err).Str("file", path).Msg("adopt: malformed archive manifest, skipping")
			continue
		}
		rec := RepoRecord{
			Name:      info.Name,
			State:     RepoArchived,
			ArchiveID: info.ID,
			OriginURL: info.Origin,
		}
		if ts, perr := time.Parse(time.RFC3339Nano, info.ArchivedAt); perr == nil {
			rec.ArchivedAt = ts
		}
		if rec.Name == "" {
			rec.Name = info.ID
		}
		if rec.ArchiveID == "" {
			// Without an id the row can never be addressed by Restore/Purge, and
			// (name, archive_id) would collide with an active repo of the same
			// name. Fall back to the manifest's filename stem, which IS the id.
			rec.ArchiveID = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		if err := reg.Upsert(rec); err != nil {
			return adopted, err
		}
		adopted++
	}

	log.Info().Int("count", adopted).Msg("adopted repos from filesystem into control.db registry")
	return adopted, nil
}
