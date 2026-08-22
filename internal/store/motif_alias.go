package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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

// spelling is one distinct motif string in the corpus, with its live df.
type spelling struct {
	name string
	df   int
}

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
// Judge merges ARE preserved: they are read from motif_judge_merges and
// overlaid on top of the mechanical grouping. The alternative — recomputing
// them — would make every review session re-judge the whole vocabulary, which
// turns §3.1's "one bounded prompt" into a cost that grows with the corpus.
//
// That does not make this function history-dependent in the sense MN3 rules
// out. Its output is a pure function of (live facts, recorded judge
// decisions), both of which are inspectable; a judge decision is a DECISION,
// the same shape as Phase 0's restatement_verdicts, not a derivation. Dropping
// motif_judge_merges costs re-judging and nothing else.
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

	// The judge overlay. Merges name CLUSTER KEYS, so they are read before the
	// grouping below is finalised and applied by union-find: a merge of A and B
	// makes every member of both one cluster, and A~B plus B~C makes all three
	// one, which is what the judge asserted transitively even though it only
	// ever compared pairs.
	//
	// A merge whose keys are not present in this corpus's vocabulary simply
	// finds nothing to union — that is how a stale decision goes inert without
	// anything cleaning it up, and why a retired spelling is never resurrected.
	merges, err := mi.judgeMerges(ctx, branchID)
	if err != nil {
		return err
	}

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
	// Apply the overlay: union the mechanical groups the judge joined, so the
	// election below runs over the UNION's members and elects one
	// representative for the whole thing. mergedKeys names the clusters that
	// exist only because of a decision, so their rows can record method
	// 'judge' and stay auditable.
	groups, mergedKeys, rationales := applyJudgeMerges(groups, merges)

	// Then elect, per cluster. Two identities come out, and the difference
	// matters:
	//   canonical_id — the highest-df member spelling, ties broken
	//                  lexicographically. DISPLAYED. It flips as usage shifts,
	//                  which is correct for a label.
	//   cluster_key  — the grouping key. STABLE under df change. Anything that
	//                  must survive across sessions (definitions, above all)
	//                  keys on this, or a representative flip orphans state
	//                  that is still valid for its cluster.
	// Both are deterministic, so a rebuild on an unchanged corpus reproduces
	// this exactly.
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
			`INSERT INTO motif_aliases(branch_id, motif, canonical_id, cluster_key, method, rationale)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			branchID, motif, a.canonical, a.clusterKey,
			methodFor(a.clusterKey, mergedKeys), rationales[a.clusterKey]); err != nil {
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

// judgeMergePair is one recorded decision, in canonical (key_a < key_b) order.
type judgeMergePair struct{ a, b, rationale string }

// judgeMerges reads this branch's recorded judge decisions.
func (mi *motifIndex) judgeMerges(ctx context.Context, branchID int64) ([]judgeMergePair, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT key_a, key_b, rationale FROM motif_judge_verdicts
		  WHERE branch_id = ? AND merged = 1`, branchID)
	if err != nil {
		return nil, fmt.Errorf("judgeMerges: %w", err)
	}
	defer rows.Close()
	var out []judgeMergePair
	for rows.Next() {
		var p judgeMergePair
		if err := rows.Scan(&p.a, &p.b, &p.rationale); err != nil {
			return nil, fmt.Errorf("judgeMerges: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// applyJudgeMerges unions the mechanical groups that recorded decisions joined.
//
// Union-find rather than a single pass, because merges compose: a judge shown
// A-B and then B-C has asserted that all three name one mechanism, even though
// it never compared A with C. Counting df over anything less than the closure
// would undercount the very mechanism the merges established.
//
// The surviving key is min() of the union's keys — deterministic, independent
// of df, and therefore stable in exactly the way T2's definitions need
// (designer rider, 2026-08-21).
//
// Merges naming keys this corpus no longer has are skipped rather than
// resurrected: `find` only knows keys present in groups.
func applyJudgeMerges(groups map[string][]spelling, merges []judgeMergePair) (map[string][]spelling, map[string]struct{}, map[string]string) {
	if len(merges) == 0 {
		return groups, nil, nil
	}
	parent := make(map[string]string, len(groups))
	var find func(string) string
	find = func(k string) string {
		p, ok := parent[k]
		if !ok || p == k {
			return k
		}
		root := find(p)
		parent[k] = root // path compression
		return root
	}
	union := func(x, y string) {
		rx, ry := find(x), find(y)
		if rx == ry {
			return
		}
		// Smaller key wins, so the survivor is min() over the whole union
		// regardless of the order decisions were recorded in.
		if ry < rx {
			rx, ry = ry, rx
		}
		parent[ry] = rx
	}
	var applied []judgeMergePair
	for _, m := range merges {
		// Both endpoints must still exist in the live vocabulary. A decision
		// about vocabulary that has left the corpus is inert, not an error.
		if _, okA := groups[m.a]; !okA {
			continue
		}
		if _, okB := groups[m.b]; !okB {
			continue
		}
		union(m.a, m.b)
		applied = append(applied, m)
	}
	merged := make(map[string][]spelling, len(groups))
	mergedKeys := map[string]struct{}{}
	for key, members := range groups {
		root := find(key)
		merged[root] = append(merged[root], members...)
		if root != key {
			mergedKeys[root] = struct{}{}
		}
	}
	// Attribute each applied merge's rationale to the cluster it produced, so
	// the audit trail lands where method='judge' is read. A cluster formed by
	// several merges carries all their reasons, joined and deduplicated by
	// order of application.
	rationales := map[string]string{}
	for _, m := range applied {
		root := find(m.a)
		if m.rationale == "" {
			continue
		}
		if existing := rationales[root]; existing != "" {
			if !strings.Contains(existing, m.rationale) {
				rationales[root] = existing + "; " + m.rationale
			}
			continue
		}
		rationales[root] = m.rationale
	}
	return merged, mergedKeys, rationales
}

// methodFor reports how a cluster came to exist, for the alias row's audit
// trail: 'judge' if a recorded decision joined it, 'canonical' if the
// mechanical layer produced it alone.
func methodFor(clusterKey string, mergedKeys map[string]struct{}) string {
	if _, ok := mergedKeys[clusterKey]; ok {
		return aliasMethodJudge
	}
	return aliasMethodCanonical
}

// AliasRow is one resolved spelling, with the audit trail of how its cluster
// came to exist.
type AliasRow struct {
	CanonicalID string
	ClusterKey  string
	Method      string
	Rationale   string
}

// pairKey is the canonical identity of an unordered cluster pair, so A-B and
// B-A are one thing everywhere: the verdict table's primary key, the pair
// selector's exclusion set, and any log line about either.
func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}

// clusterMembership returns each cluster key's members, sorted and joined —
// the fingerprint a verdict is recorded against.
//
// A verdict binds only while both sides still mean what they meant. A cluster
// that has gained or lost a spelling is not the cluster the judge was shown,
// and asking again is correct rather than wasteful. Structural expiry, with
// nothing to invalidate and no cleanup job: the same reasoning as Phase 0
// keying its verdicts on content-addressed fact ids.
func (mi *motifIndex) clusterMembership(ctx context.Context, branchID int64) (map[string]string, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT cluster_key, motif FROM motif_aliases WHERE branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("clusterMembership: %w", err)
	}
	defer rows.Close()
	members := map[string][]string{}
	for rows.Next() {
		var key, motif string
		if err := rows.Scan(&key, &motif); err != nil {
			return nil, fmt.Errorf("clusterMembership: scan: %w", err)
		}
		members[key] = append(members[key], motif)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(members))
	for key, ms := range members {
		sort.Strings(ms)
		out[key] = strings.Join(ms, ",")
	}
	return out, nil
}

// AliasRows returns the full alias table with its audit columns.
func (mi *motifIndex) AliasRows(ctx context.Context, branch string) (map[string]AliasRow, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("AliasRows: %w", err)
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT motif, canonical_id, cluster_key, method, rationale
		   FROM motif_aliases WHERE branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("AliasRows: %w", err)
	}
	defer rows.Close()
	out := map[string]AliasRow{}
	for rows.Next() {
		var motif string
		var r AliasRow
		if err := rows.Scan(&motif, &r.CanonicalID, &r.ClusterKey, &r.Method, &r.Rationale); err != nil {
			return nil, fmt.Errorf("AliasRows: scan: %w", err)
		}
		out[motif] = r
	}
	return out, rows.Err()
}

// AnsweredPairs returns the cluster pairs whose verdict STILL BINDS — recorded
// against a membership both sides still have. The pair selector subtracts these
// so a session never re-asks a question it has an answer to.
//
// A verdict whose membership has moved is simply absent, which re-eligibilizes
// the pair without deleting anything: the record stays as history, it just
// stops binding.
func (mi *motifIndex) AnsweredPairs(ctx context.Context, branch string) (map[string]struct{}, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("AnsweredPairs: %w", err)
	}
	membership, err := mi.clusterMembership(ctx, branchID)
	if err != nil {
		return nil, err
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT key_a, key_b, members_a, members_b
		   FROM motif_judge_verdicts WHERE branch_id = ?`, branchID)
	if err != nil {
		return nil, fmt.Errorf("AnsweredPairs: %w", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var keyA, keyB, memA, memB string
		if err := rows.Scan(&keyA, &keyB, &memA, &memB); err != nil {
			return nil, fmt.Errorf("AnsweredPairs: scan: %w", err)
		}
		// Both sides must still hold the membership the judge answered about.
		// A merged pair's keys have since become one cluster, so it is absent
		// here too — correct, since the overlay already unified it and the
		// selector will never see the two as separate clusters again.
		if membership[keyA] != memA || membership[keyB] != memB {
			continue
		}
		out[pairKey(keyA, keyB)] = struct{}{}
	}
	return out, rows.Err()
}

// RecordJudgeMerge records that the LLM clustering pass judged two clusters to
// name the same mechanism. Takes MOTIF SPELLINGS (what the judge was shown)
// and stores their CLUSTER KEYS (what is stable across sessions).
//
// The decision takes effect at the next RebuildAliases, not immediately: the
// mechanical layer and the overlay are applied together so their result is one
// deterministic function of (facts, decisions) rather than an accumulation of
// partial edits.
func (mi *motifIndex) RecordJudgeMerge(ctx context.Context, branch, motifA, motifB, rationale string) error {
	if strings.TrimSpace(rationale) == "" {
		// Enforced here, not asked for in the prompt and hoped for. A merge
		// whose shared mechanism nobody could name is exactly the hallucinated
		// merge the guard exists to stop, and over-merge is the invisible
		// failure — nothing downstream can tell that two mechanisms were fused.
		return fmt.Errorf("RecordJudgeMerge: a merge must name the shared mechanism")
	}
	return mi.recordVerdict(ctx, branch, motifA, motifB, true, rationale)
}

// RecordJudgeDecline records that the judge was shown a pair and said no.
//
// Recording the NO is what makes the pass incremental in both directions.
// Without it, merges are cheap but rejections are re-litigated every session,
// and a stable corpus spends its whole slot budget re-asking answered
// questions.
func (mi *motifIndex) RecordJudgeDecline(ctx context.Context, branch, motifA, motifB string) error {
	return mi.recordVerdict(ctx, branch, motifA, motifB, false, "")
}

func (mi *motifIndex) recordVerdict(ctx context.Context, branch, motifA, motifB string, merged bool, rationale string) error {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("recordVerdict: %w", err)
	}
	keyA, err := mi.ClusterKey(ctx, branch, motifA)
	if err != nil {
		return err
	}
	keyB, err := mi.ClusterKey(ctx, branch, motifB)
	if err != nil {
		return err
	}
	if keyA == keyB {
		return nil // already one cluster; nothing to decide
	}
	membership, err := mi.clusterMembership(ctx, branchID)
	if err != nil {
		return err
	}
	memA, memB := membership[keyA], membership[keyB]
	if keyB < keyA {
		keyA, keyB = keyB, keyA
		memA, memB = memB, memA
	}
	if _, err := conn(ctx, mi.rh.db).ExecContext(ctx,
		`INSERT INTO motif_judge_verdicts
		     (branch_id, key_a, key_b, judged_at, merged, rationale, members_a, members_b)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(branch_id, key_a, key_b) DO UPDATE SET
		     judged_at = excluded.judged_at,
		     merged    = excluded.merged,
		     rationale = excluded.rationale,
		     members_a = excluded.members_a,
		     members_b = excluded.members_b`,
		branchID, keyA, keyB, time.Now().UTC().Format(time.RFC3339),
		merged, rationale, memA, memB); err != nil {
		return fmt.Errorf("recordVerdict: %w", err)
	}
	return nil
}
