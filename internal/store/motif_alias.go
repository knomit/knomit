package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/textnorm"
)

// Alias resolution, mechanical half (blueprint §3.1 step 1).
//
// The corpus's distinct motif spellings are grouped into clusters, and each
// cluster is known by a canonical id. This file implements ONLY the
// deterministic layer: stemming and canonicalization. The LLM-judged pass that
// merges clusters string similarity cannot reach is layered on top and records
// itself with method "judge", so the two can be rebuilt independently.
//
// Everything here is DERIVED STATE (MN3). The authored strings in
// facts.motifs and fact_motifs are the claim; nothing in this file writes back
// into a fact, and dropping motif_aliases entirely costs nothing but a
// rebuild.

// Alias resolution methods, as stored in motif_aliases.method.
const (
	// aliasMethodCanonical is the mechanical stem/canonicalize layer below.
	aliasMethodCanonical = "canonical"
	// aliasMethodJudge marks a merge the LLM clustering pass made.
	aliasMethodJudge = "judge"
)

// motifIndex implements MotifIndex.
type motifIndex struct{ rh *repoHandler }

// groupingKey is the mechanical cluster key for a motif spelling: its
// canonicalized tokens, stemmed, SORTED and rejoined.
//
// Sorted, so the key is a token MULTISET and word order cannot split a
// cluster — "atomic-write" and "write-atomic" name the same mechanism.
//
// Stemming comes from textnorm.Tokens, which already returns STEMMED tokens
// (see its doc) — so "silent-fallbacks" and "silent-fallback" group without a
// second Stem call here. An earlier draft called textnorm.Stem again on each
// token; it was invisible because stemming is idempotent, and it was caught by
// sabotaging it and watching the test still pass. Reaching for the shared
// matcher rather than re-deriving is also what invariant 7a6af15e requires.
//
// This key is internal. It is deliberately NOT the canonical id: sorting reads
// badly ("fallback-silent"), and the id is shown to readers in the §6 explain
// surface and to an LLM in the backfill prompt. The id is a real member
// spelling; see RebuildAliases.
func groupingKey(motif string) string {
	toks := textnorm.Tokens(textnorm.Canonicalize(motif))
	stemmed := make([]string, 0, len(toks))
	for _, t := range toks {
		if t == "" {
			continue
		}
		stemmed = append(stemmed, t)
	}
	sort.Strings(stemmed)
	return strings.Join(stemmed, "-")
}

// RebuildAliases recomputes the mechanical alias layer for branch from the
// live corpus, replacing whatever was there.
//
// Level-triggered by construction: it reads the current vocabulary and writes
// the current answer, so a spelling that no live fact carries any more simply
// does not reappear. That matters — a vocabulary that only grows would compute
// df over clusters containing members nothing carries.
//
// Judge merges are NOT preserved across a rebuild. They are the LLM layer's
// business to re-establish; keeping them here would make this function's
// output depend on history rather than on the corpus, which is exactly what
// "rebuildable derived state" rules out.
func (mi *motifIndex) RebuildAliases(ctx context.Context, branch string) error {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("RebuildAliases: %w", err)
	}

	// df per spelling over LIVE facts only, so the representative below is the
	// spelling the corpus actually uses today.
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx, `
		SELECT m.motif, COUNT(DISTINCT bf.path) AS df
		  FROM branch_facts bf
		  JOIN fact_motifs m ON m.fact_id = bf.fact_id
		 WHERE bf.branch_id = ?
		 GROUP BY m.motif`, branchID)
	if err != nil {
		return fmt.Errorf("RebuildAliases: vocabulary: %w", err)
	}
	type spelling struct {
		name string
		df   int
	}
	var vocab []spelling
	for rows.Next() {
		var s spelling
		if err := rows.Scan(&s.name, &s.df); err != nil {
			rows.Close()
			return fmt.Errorf("RebuildAliases: scan: %w", err)
		}
		vocab = append(vocab, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("RebuildAliases: vocabulary: %w", err)
	}

	// Group, then elect a representative per group: highest df, ties broken
	// lexicographically so the result is deterministic and a rebuild on an
	// unchanged corpus reproduces it exactly.
	groups := map[string][]spelling{}
	for _, s := range vocab {
		k := groupingKey(s.name)
		if k == "" {
			// A motif whose every token normalized away has no mechanical
			// cluster to join. It stays its own singleton via the
			// resolve-to-itself fallback in CanonicalID rather than joining a
			// bogus empty-key group with every other such motif.
			continue
		}
		groups[k] = append(groups[k], s)
	}
	// Two identities per cluster, and the difference matters:
	//   canonical_id — the highest-df member spelling. DISPLAYED. It flips as
	//                  usage shifts, which is correct for a label.
	//   cluster_key  — the mechanical grouping key. STABLE under df change.
	//                  Anything that must survive across sessions (definitions,
	//                  above all) keys on this, or a representative flip
	//                  orphans state that is still valid for its cluster.
	type assignment struct{ canonical, clusterKey string }
	assignments := make(map[string]assignment, len(vocab))
	for key, members := range groups {
		sort.Slice(members, func(i, j int) bool {
			if members[i].df != members[j].df {
				return members[i].df > members[j].df
			}
			return members[i].name < members[j].name
		})
		rep := members[0].name
		for _, m := range members {
			assignments[m.name] = assignment{canonical: rep, clusterKey: key}
		}
	}

	ctx, tx, ownTx, err := beginTxIfNeeded(ctx, mi.rh.db)
	if err != nil {
		return fmt.Errorf("RebuildAliases: begin: %w", err)
	}
	if ownTx {
		defer tx.Rollback()
	}
	if _, err := conn(ctx, mi.rh.db).ExecContext(ctx,
		`DELETE FROM motif_aliases WHERE branch_id = ?`, branchID); err != nil {
		return fmt.Errorf("RebuildAliases: clear: %w", err)
	}
	for motif, a := range assignments {
		if _, err := conn(ctx, mi.rh.db).ExecContext(ctx,
			`INSERT INTO motif_aliases(branch_id, motif, canonical_id, cluster_key, method)
			 VALUES (?, ?, ?, ?, ?)`,
			branchID, motif, a.canonical, a.clusterKey, aliasMethodCanonical); err != nil {
			return fmt.Errorf("RebuildAliases: insert %q: %w", motif, err)
		}
	}
	if ownTx {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("RebuildAliases: commit: %w", err)
		}
	}
	return nil
}

// CanonicalID resolves one motif spelling to its cluster's canonical id.
//
// An unrecognised spelling resolves to ITSELF, not to an error or an empty
// string. That makes "no alias table yet" indistinguishable from "every motif
// is its own cluster", which is the correct degenerate reading and spares
// every consumer — df, matching, the read surfaces — a special case for a
// corpus whose vocabulary has not been resolved yet.
func (mi *motifIndex) CanonicalID(ctx context.Context, branch, motif string) (string, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("CanonicalID: %w", err)
	}
	var id string
	err = conn(ctx, mi.rh.db).QueryRowContext(ctx,
		`SELECT canonical_id FROM motif_aliases WHERE branch_id = ? AND motif = ?`,
		branchID, motif,
	).Scan(&id)
	if err != nil {
		// Includes sql.ErrNoRows: unresolved means its own singleton.
		return motif, nil //nolint:nilerr // documented fallback, see above
	}
	return id, nil
}

// AliasTable returns the whole spelling → canonical id mapping for branch.
// Used by the health metrics, the backfill hint generator, and the MN3
// rebuild-reproducibility test.
func (mi *motifIndex) AliasTable(ctx context.Context, branch string) (map[string]string, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("AliasTable: %w", err)
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT motif, canonical_id FROM motif_aliases WHERE branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("AliasTable: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var motif, id string
		if err := rows.Scan(&motif, &id); err != nil {
			return nil, fmt.Errorf("AliasTable: scan: %w", err)
		}
		out[motif] = id
	}
	return out, rows.Err()
}

// ClusterKey returns the STABLE identity of a spelling's cluster.
//
// Use this, never CanonicalID, as the key for anything that must survive
// across sessions — definitions above all. CanonicalID is the highest-df
// member spelling, so it flips when usage shifts; the cluster it names has not
// changed, and state keyed to the old representative would be orphaned by a
// change that meant nothing.
//
// An unresolved spelling returns its own mechanical grouping key, which is
// what it would be assigned by a rebuild — so a corpus whose aliases have not
// been built yet still produces stable, correct keys.
func (mi *motifIndex) ClusterKey(ctx context.Context, branch, motif string) (string, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("ClusterKey: %w", err)
	}
	var key string
	err = conn(ctx, mi.rh.db).QueryRowContext(ctx,
		`SELECT cluster_key FROM motif_aliases WHERE branch_id = ? AND motif = ?`,
		branchID, motif,
	).Scan(&key)
	if err != nil {
		return groupingKey(motif), nil //nolint:nilerr // documented fallback
	}
	return key, nil
}
