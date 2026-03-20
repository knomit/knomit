// Replay algorithm: copies local facts into a target store's agent branch.
// Used when two knomit instances with disjoint histories connect.
package git

import (
	"database/sql"
	"fmt"
	"io"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"knomit/internal/store"
)

// ConflictStrategy determines how shared-path conflicts are resolved during replay.
type ConflictStrategy string

const (
	StrategyLocalWins  ConflictStrategy = "local_wins"
	StrategyRemoteWins ConflictStrategy = "remote_wins"
)

// ReplayConfig controls replay behavior.
type ReplayConfig struct {
	Strategy      ConflictStrategy
	AgentBranch   string
	DefaultBranch string
	OnProgress    func(current, total int)
}

// ReplayResult reports what happened during replay.
type ReplayResult struct {
	TotalFacts           int
	FromLocal            int
	FromRemote           int
	Overwrites           int
	RefsResolvedFromHist int
	DanglingRefsDropped  int
}

// Replay copies local facts into a target store's agent branch.
// It iterates facts from localDB (newest-first, deduped by path), reads their
// blob content from the local store, resolves dead refs, and writes each fact
// to the target store.
func Replay(local *Store, localDB *sql.DB, target *Store, cfg ReplayConfig) (*ReplayResult, error) {
	if cfg.AgentBranch == "" {
		return nil, fmt.Errorf("Replay: AgentBranch must be set")
	}
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = "main"
	}
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyLocalWins
	}

	// 1. Create agent branch from default branch in target store.
	defaultRefName := plumbing.NewBranchReferenceName(cfg.DefaultBranch)
	defaultRef, err := target.storer.Reference(defaultRefName)
	if err != nil {
		return nil, fmt.Errorf("Replay: resolve default branch %q: %w", cfg.DefaultBranch, err)
	}

	agentRefName := plumbing.NewBranchReferenceName(cfg.AgentBranch)
	if err := target.storer.SetReference(plumbing.NewHashReference(agentRefName, defaultRef.Hash())); err != nil {
		return nil, fmt.Errorf("Replay: create agent branch: %w", err)
	}
	if err := target.storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, agentRefName)); err != nil {
		return nil, fmt.Errorf("Replay: set HEAD: %w", err)
	}
	target.branch = cfg.AgentBranch
	target.agentID = deriveAgentID(cfg.AgentBranch)

	log.Debug().Str("agent_branch", cfg.AgentBranch).Str("from", cfg.DefaultBranch).Msg("replay: created agent branch")

	// 2. Open fact iterator from localDB.
	iter, err := store.NewFactsIter(localDB)
	if err != nil {
		return nil, fmt.Errorf("Replay: open facts iterator: %w", err)
	}
	defer iter.Close()

	// Collect all facts first so we know the total count for progress reporting.
	type localFact struct {
		path     string
		blobHash string
	}
	var facts []localFact
	for {
		row, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("Replay: iterate facts: %w", err)
		}
		if row == nil {
			break
		}
		facts = append(facts, localFact{path: row.Path, blobHash: row.BlobHash})
	}

	// 3. Count remote facts for stats.
	remotePaths, err := target.ListAll()
	if err != nil {
		return nil, fmt.Errorf("Replay: list remote facts: %w", err)
	}
	remotePathSet := make(map[string]bool, len(remotePaths))
	for _, p := range remotePaths {
		remotePathSet[p] = true
	}

	// Also build a set of local fact paths for dead ref resolution.
	localPathSet := make(map[string]bool, len(facts))
	for _, f := range facts {
		localPathSet[f.path] = true
	}

	result := &ReplayResult{
		TotalFacts: len(facts) + len(remotePaths),
		FromRemote: len(remotePaths),
	}

	// 4. For each local fact, replay into target.
	for i, f := range facts {
		// Read blob content from local git store.
		content, err := readBlobByHash(local, f.blobHash)
		if err != nil {
			return nil, fmt.Errorf("Replay: read blob %s for %s: %w", f.blobHash, f.path, err)
		}

		// Check if path exists in target (shared path).
		isShared := remotePathSet[f.path]
		if isShared {
			switch cfg.Strategy {
			case StrategyRemoteWins:
				// Skip — remote version is kept.
				if cfg.OnProgress != nil {
					cfg.OnProgress(i+1, len(facts))
				}
				continue
			case StrategyLocalWins:
				result.Overwrites++
				// Overwrite count tracked, but not in FromRemote reduction —
				// the remote fact is still counted in TotalFacts.
			}
		}

		// Resolve dead refs.
		resolvedContent, resolvedCount, droppedCount, err := resolveDeadRefs(local, content, f.path, localPathSet, remotePathSet)
		if err != nil {
			log.Warn().Err(err).Str("path", f.path).Msg("replay: dead ref resolution failed, using original content")
			resolvedContent = content
		}
		result.RefsResolvedFromHist += resolvedCount
		result.DanglingRefsDropped += droppedCount

		// Write fact to target store and commit.
		msg := fmt.Sprintf("replay: %s", f.path)
		if _, _, err := target.WriteFile(f.path, resolvedContent, msg, "replay"); err != nil {
			return nil, fmt.Errorf("Replay: write %s to target: %w", f.path, err)
		}
		result.FromLocal++

		if cfg.OnProgress != nil {
			cfg.OnProgress(i+1, len(facts))
		}
	}

	log.Info().
		Int("from_local", result.FromLocal).
		Int("from_remote", result.FromRemote).
		Int("overwrites", result.Overwrites).
		Int("refs_resolved", result.RefsResolvedFromHist).
		Int("refs_dropped", result.DanglingRefsDropped).
		Msg("replay complete")

	return result, nil
}

// readBlobByHash reads the content of a blob by its hash from the store's git repo.
func readBlobByHash(s *Store, hashStr string) (string, error) {
	h := plumbing.NewHash(hashStr)
	blob, err := s.repo.BlobObject(h)
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("readBlobByHash: read: %w", err)
	}
	return string(b), nil
}

// replayFrontmatter is the YAML structure for parsing refs from fact frontmatter.
type replayFrontmatter struct {
	Type       string   `yaml:"type"`
	Domain     []string `yaml:"domain"`
	Confidence float64  `yaml:"confidence"`
	Sources    int      `yaml:"sources"`
	Entities   []string `yaml:"entities"`
	Refs       []string `yaml:"refs"`
}

// resolveDeadRefs checks each ref in a fact's frontmatter and resolves dead local refs.
// Returns: modified content, count of resolved refs, count of dropped refs.
func resolveDeadRefs(local *Store, content, path string, localPathSet, remotePathSet map[string]bool) (string, int, int, error) {
	fm, yamlBlock, body, err := parseFrontmatterRefs(content)
	if err != nil {
		// Not a valid frontmatter file — return as-is.
		return content, 0, 0, nil
	}

	if len(fm.Refs) == 0 {
		return content, 0, 0, nil
	}

	var newRefs []string
	resolvedCount := 0
	droppedCount := 0

	for _, ref := range fm.Refs {
		// External URL — always keep.
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			newRefs = append(newRefs, ref)
			continue
		}

		// Local path that exists in local store or target — keep.
		if localPathSet[ref] || remotePathSet[ref] {
			newRefs = append(newRefs, ref)
			continue
		}

		// Dead local ref — try to resolve from history (1 level deep).
		externalRefs, err := extractExternalRefsFromHistory(local, ref)
		if err != nil {
			log.Debug().Err(err).Str("ref", ref).Str("fact", path).Msg("replay: could not resolve dead ref from history")
			droppedCount++
			continue
		}

		if len(externalRefs) == 0 {
			// Dead fact had no external refs — drop.
			droppedCount++
			continue
		}

		// Graft external refs onto the current fact.
		newRefs = append(newRefs, externalRefs...)
		resolvedCount += len(externalRefs)
	}

	if resolvedCount == 0 && droppedCount == 0 {
		return content, 0, 0, nil
	}

	// Rebuild the content with updated refs.
	fm.Refs = newRefs
	return rebuildContent(fm, yamlBlock, body), resolvedCount, droppedCount, nil
}

// extractExternalRefsFromHistory looks up the last version of a deleted fact in
// local git history and extracts its external (http/https) refs.
func extractExternalRefsFromHistory(local *Store, deadPath string) ([]string, error) {
	// Walk the commit log for this path to find its last version.
	headRef, err := local.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: head: %w", err)
	}

	logIter, err := local.repo.Log(&gogit.LogOptions{
		From:     headRef.Hash(),
		FileName: &deadPath,
	})
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: log: %w", err)
	}
	defer logIter.Close()

	// Walk commits that touched this path and find one where the file actually
	// exists in the tree (the first hit may be the delete commit).
	var deadContent string
	for {
		c, err := logIter.Next()
		if err != nil {
			return nil, fmt.Errorf("extractExternalRefsFromHistory: no commit with content found for %q", deadPath)
		}
		tree, err := c.Tree()
		if err != nil {
			continue
		}
		f, err := tree.File(deadPath)
		if err != nil {
			continue
		}
		deadContent, err = f.Contents()
		if err != nil {
			continue
		}
		break
	}

	deadFM, _, _, err := parseFrontmatterRefs(deadContent)
	if err != nil {
		return nil, fmt.Errorf("extractExternalRefsFromHistory: parse frontmatter: %w", err)
	}

	var externalRefs []string
	for _, ref := range deadFM.Refs {
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			externalRefs = append(externalRefs, ref)
		}
	}
	return externalRefs, nil
}

// parseFrontmatterRefs parses YAML frontmatter from fact content.
// Returns the parsed frontmatter, the raw YAML block, and the body after the closing ---.
func parseFrontmatterRefs(content string) (*replayFrontmatter, string, string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")

	if !strings.HasPrefix(content, "---\n") {
		return nil, "", "", fmt.Errorf("missing opening frontmatter delimiter")
	}

	rest := content[4:]
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		return nil, "", "", fmt.Errorf("missing closing frontmatter delimiter")
	}

	yamlBlock := rest[:closeIdx]
	body := rest[closeIdx+4:] // skip "\n---"

	var fm replayFrontmatter
	if err := yaml.Unmarshal([]byte(yamlBlock), &fm); err != nil {
		return nil, "", "", fmt.Errorf("yaml parse: %w", err)
	}

	if fm.Refs == nil {
		fm.Refs = []string{}
	}

	return &fm, yamlBlock, body, nil
}

// rebuildContent reconstructs a fact file with updated frontmatter refs.
func rebuildContent(fm *replayFrontmatter, _ string, body string) string {
	var sb strings.Builder
	sb.WriteString("---\n")

	if fm.Type != "" {
		sb.WriteString("type: ")
		sb.WriteString(fm.Type)
		sb.WriteString("\n")
	}
	sb.WriteString("domain: ")
	sb.WriteString(serializeRefList(fm.Domain))
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("confidence: %g\n", fm.Confidence))
	sb.WriteString(fmt.Sprintf("sources: %d\n", fm.Sources))

	sb.WriteString("entities: ")
	sb.WriteString(serializeRefList(fm.Entities))
	sb.WriteString("\n")

	sb.WriteString("refs: ")
	sb.WriteString(serializeRefList(fm.Refs))
	sb.WriteString("\n")

	sb.WriteString("---\n")
	// body includes the leading \n from the closing delimiter parse
	sb.WriteString(body)

	return sb.String()
}

// serializeRefList renders a []string as a YAML inline list: [a, b, c] or [].
func serializeRefList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, item := range items {
		if strings.ContainsAny(item, ",]\"") {
			escaped := strings.ReplaceAll(item, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			quoted[i] = `"` + escaped + `"`
		} else {
			quoted[i] = item
		}
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
