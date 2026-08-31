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

// motifClusterKeyExpr renders the ONE SQL expression that keys a motif
// spelling to its cluster, for the readers that must agree with each other.
//
// Three steps, and each is a posture decision rather than defensiveness:
//   - the STORED key, when the vocabulary has been resolved;
//   - else the key a rebuild WOULD compute (knomit_motif_key is groupingKey),
//     so a corpus with facts written since the last rebuild reads the way it
//     will read a moment later rather than a different way in the meantime;
//   - else the spelling itself, for a motif whose every token normalizes away.
//     Those have no mechanical cluster to join, and letting them share the
//     empty key would fuse unrelated motifs into one; RebuildAliases makes the
//     same call by skipping them.
//
// Shared as TEXT because the alternative is three hand-written COALESCEs that
// agree until one of them is edited. The caller supplies the motif column
// (queries name it differently) and must alias motif_aliases as `a`.
func motifClusterKeyExpr(motifCol string) string {
	return `COALESCE(NULLIF(a.cluster_key, ''), NULLIF(knomit_motif_key(` + motifCol + `), ''), ` + motifCol + `)`
}

// electCanonical picks a cluster's DISPLAYED representative: the highest-df
// member spelling, ties broken lexicographically.
//
// One definition, two callers, deliberately. RebuildAliases elects the
// representative it STORES, and Clusters must elect the same one for a cluster
// the alias table does not cover yet — otherwise an unresolved corpus is named
// one way by the reader and another way by the next rebuild. Two functions that
// happen to agree today would drift; sharing the derivation is what makes them
// agree by construction (the reasoning invariant 7a6af15e states).
//
// Deterministic on ties, so a rebuild on an unchanged corpus reproduces its
// previous answer exactly.
func electCanonical(members []spelling) string {
	if len(members) == 0 {
		return ""
	}
	best := members[0]
	for _, m := range members[1:] {
		if m.df > best.df || (m.df == best.df && m.name < best.name) {
			best = m
		}
	}
	return best.name
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
		rep := electCanonical(members)
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
	// Retire definitions whose cluster no longer exists. A judge merge leaves
	// the ABSORBED key with no members, and its definition row would otherwise
	// linger forever — invisible, since nothing queues or serves a cluster that
	// is not in the vocabulary, but accumulating and liable to be resurrected
	// if that key ever came back meaning something else.
	//
	// The SURVIVOR's definition is deliberately kept: it is the interim the
	// designer ruled for, still approximately right for the union, and
	// ClustersNeedingDefinition queues its refresh.
	if _, err := conn(ctx, mi.rh.db).ExecContext(ctx, `
		DELETE FROM motif_definitions
		 WHERE branch_id = ?
		   AND cluster_key NOT IN (
		       SELECT DISTINCT cluster_key FROM motif_aliases WHERE branch_id = ?)`,
		branchID, branchID); err != nil {
		return fmt.Errorf("RebuildAliases: retire orphaned definitions: %w", err)
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
	parent := map[string]string{}
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
		if ry < rx {
			rx, ry = ry, rx
		}
		parent[ry] = rx
	}

	// Union EVERY recorded merge, including ones naming vocabulary the corpus
	// no longer carries.
	//
	// The earlier version skipped a merge unless both endpoints were live,
	// which SPLIT a transitive chain when its middle retired: a judge that
	// merged A~B and B~C asserted all three name one mechanism, and retiring
	// B's spelling silently undid that — leaving A and C in separate clusters
	// with nothing recording that a judge had said otherwise. The merges are
	// decisions about MECHANISMS; a spelling leaving the corpus does not
	// withdraw one.
	var applied []judgeMergePair
	for _, m := range merges {
		union(m.a, m.b)
		applied = append(applied, m)
	}

	// Group the LIVE keys by their component, then key each component by the
	// smallest key that is actually live.
	//
	// That last part is what keeps a dead key from naming a live cluster. Union
	// alone would let a retired endpoint win min() and identify a cluster no
	// spelling has — measured earlier in this phase: "silent-fallback" came
	// back keyed "degradation-quiet" after that spelling had left the corpus.
	liveKeysByRoot := map[string][]string{}
	for key := range groups {
		root := find(key)
		liveKeysByRoot[root] = append(liveKeysByRoot[root], key)
	}
	survivorOf := map[string]string{}
	for root, keys := range liveKeysByRoot {
		sort.Strings(keys)
		survivorOf[root] = keys[0]
	}

	merged := make(map[string][]spelling, len(groups))
	mergedKeys := map[string]struct{}{}
	for key, members := range groups {
		survivor := survivorOf[find(key)]
		merged[survivor] = append(merged[survivor], members...)
		if survivor != key {
			mergedKeys[survivor] = struct{}{}
		}
	}

	// Attribute each applied merge's rationale to the cluster it produced.
	rationales := map[string]string{}
	for _, m := range applied {
		root, ok := survivorOf[find(m.a)]
		if !ok || m.rationale == "" {
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
		// Enforced here, not asked for in the prompt and hoped for: a rule the
		// caller can decline to follow is a convention, not a guard. A merge
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

// MotifCluster is one resolved cluster: what the judge is shown, and what the
// §6 surfaces display.
type MotifCluster struct {
	CanonicalID string   // representative spelling — DISPLAYED
	ClusterKey  string   // stable identity — KEY state on this
	Members     []string // every spelling resolving here, sorted
	DF          int      // live facts carrying any member, counted once each
	// DFTotal is DF over the WHOLE branch, which is what DF already is unless
	// ClustersUnder narrowed it to a subtree. The pair is reported together
	// because the two numbers answer different questions and a scoped view
	// needs both: DF says how much of this shape is HERE, DFTotal says how much
	// the pivot will return — and the pivot deliberately leaves the path behind.
	DFTotal int
}

// Clusters returns this branch's resolved motif vocabulary, most frequent
// first, ties broken by canonical id so the order is deterministic.
//
// One row per CLUSTER, not per spelling: the vocabulary a reader or a judge
// deals in is mechanisms, and two spellings of one mechanism are one entry.
func (mi *motifIndex) Clusters(ctx context.Context, branch string) ([]MotifCluster, error) {
	return mi.ClustersUnder(ctx, branch, "")
}

// ClustersUnder is Clusters restricted to the facts under pathPrefix — the
// vocabulary of one subtree rather than of the branch.
//
// The narrowing is applied to the DF COUNTS ONLY, never to membership or to the
// canonical election. A cluster's members and its representative spelling are
// properties of the cluster, not of where you are standing: electing a
// representative from the in-scope spellings would let one folder call a
// cluster `silent-fallback` and another call the same cluster
// `quiet-degradation`, and the pivot heading — which is branch-wide — would
// then disagree with the row that opened it. So both are computed exactly as
// the unscoped call computes them, and the prefix decides only which clusters
// survive and what number each shows.
func (mi *motifIndex) ClustersUnder(ctx context.Context, branch, pathPrefix string) ([]MotifCluster, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("Clusters: %w", err)
	}
	// df counts DISTINCT carrier paths across the cluster, so a fact using two
	// spellings of one mechanism is one carrier — the same rule TokenDF applies,
	// for the same reason.
	// Driven from fact_motifs with a LEFT JOIN, not from motif_aliases — so an
	// UNRESOLVED spelling appears as its own singleton cluster rather than
	// vanishing.
	//
	// One posture everywhere (review remediation, 2026-08-23). TokenDF,
	// CanonicalID, ClusterKey and the query tiers all degrade to "every motif
	// is its own cluster" on an unresolved corpus; Clusters, VocabularyHealth
	// and CarrierTitles used to return NOTHING instead, so the same corpus
	// reported a real vocabulary through one API and an empty one through
	// another. An empty alias table means "no review session has run yet" — a
	// transient bootstrap state, never a fact about the corpus — and the
	// singleton reading is what a rebuild would produce for a corpus with no
	// aliasing anyway.
	//
	// The KEY IS COMPUTED IN SQL, via knomit_motif_key, and that is the whole
	// point rather than a detail. Grouping on the stored key and repairing the
	// blank afterwards in Go put the repair AFTER the GROUP BY, so two
	// unresolved spellings of one mechanism came back as two rows that then
	// claimed the SAME cluster key — while VocabularyHealth, which keys inside
	// its query, reported them as one. Same corpus, two vocabularies, which is
	// exactly what "one posture" was supposed to end. Any corpus with facts
	// written since the last rebuild is in this state.
	//
	// The three-step COALESCE is the singleton rule spelled out: the stored key
	// if the vocabulary is resolved, else the key a rebuild would compute, else
	// — for a motif whose every token normalizes away — the spelling itself, so
	// such motifs stay separate singletons instead of collapsing into one
	// bogus empty-key group. RebuildAliases makes the same choice by skipping
	// them.
	// The scope predicate is spelled into the query rather than appended as a
	// WHERE, because the two counts have to come out of ONE pass over the same
	// rows: a scoped list whose totals were fetched by a second query could
	// report a cluster as 3-here-of-2-everywhere the moment a write landed
	// between them. In scope for everything when no prefix is given.
	inScope := "1"
	args := []any{branchID}
	if pathPrefix != "" {
		inScope = "path LIKE ?"
		args = append(args, pathPrefix+"%")
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx, `
		WITH resolved AS (
		  SELECT bf.path AS path,
		         m.motif AS motif,
		         `+motifClusterKeyExpr("m.motif")+` AS cluster_key,
		         a.canonical_id AS canonical_id
		    FROM branch_facts bf
		    JOIN fact_motifs m ON m.fact_id = bf.fact_id
		    LEFT JOIN motif_aliases a
		           ON a.branch_id = bf.branch_id AND a.motif = m.motif
		   WHERE bf.branch_id = ?
		),
		per_motif AS (
		  SELECT cluster_key, motif,
		         COUNT(DISTINCT path) AS motif_df,
		         MAX(canonical_id) AS canonical_id
		    FROM resolved GROUP BY cluster_key, motif
		),
		per_cluster AS (
		  SELECT cluster_key,
		         COUNT(DISTINCT CASE WHEN `+inScope+` THEN path END) AS cluster_df,
		         COUNT(DISTINCT path) AS cluster_df_total
		    FROM resolved GROUP BY cluster_key
		)
		SELECT pm.cluster_key, pm.motif, pm.motif_df,
		       COALESCE(pm.canonical_id, ''), pc.cluster_df, pc.cluster_df_total
		  FROM per_motif pm
		  JOIN per_cluster pc ON pc.cluster_key = pm.cluster_key
		 WHERE pc.cluster_df > 0`, args...)
	if err != nil {
		return nil, fmt.Errorf("Clusters: %w", err)
	}
	defer rows.Close()
	// Assembled per cluster in Go, because the representative is ELECTED and
	// the election has a tiebreak SQL would have to reproduce. electCanonical
	// is the same function RebuildAliases uses, so an unresolved cluster is
	// named here exactly as the next rebuild will name it.
	type acc struct {
		members []spelling
		stored  string // canonical_id from the alias table, when resolved
		df      int
		dfTotal int
	}
	byKey := map[string]*acc{}
	var order []string
	for rows.Next() {
		var key, motif, stored string
		var motifDF, clusterDF, clusterDFTotal int
		if err := rows.Scan(&key, &motif, &motifDF, &stored, &clusterDF, &clusterDFTotal); err != nil {
			return nil, fmt.Errorf("Clusters: scan: %w", err)
		}
		a, seen := byKey[key]
		if !seen {
			a = &acc{df: clusterDF, dfTotal: clusterDFTotal}
			byKey[key] = a
			order = append(order, key)
		}
		a.members = append(a.members, spelling{name: motif, df: motifDF})
		if stored != "" {
			a.stored = stored
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]MotifCluster, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		c := MotifCluster{ClusterKey: key, CanonicalID: a.stored, DF: a.df, DFTotal: a.dfTotal}
		if c.CanonicalID == "" {
			c.CanonicalID = electCanonical(a.members)
		}
		for _, m := range a.members {
			c.Members = append(c.Members, m.name)
		}
		sort.Strings(c.Members)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DF != out[j].DF {
			return out[i].DF > out[j].DF
		}
		return out[i].CanonicalID < out[j].CanonicalID
	})
	return out, nil
}

// CarrierTitles returns up to limit titles of live facts carrying any spelling
// in the cluster, MOST RECENT FIRST.
//
// The judge sees these, and that is not decoration: string-only clustering
// demonstrably keeps adjacent-family false merges (blueprint §12-E3). Two
// motifs can read as synonyms and be carried by facts about visibly different
// mechanisms, and the titles are what makes that visible.
//
// Order is load-bearing because the list is CAPPED: whichever titles the cap
// admits are the entire evidence the judge gets, and explain's sibling list
// inherits the same ordering. Alphabetical would show a cluster's oldest,
// least representative carriers whenever their titles sorted early. This
// documented most-recent-first and ordered by title, which is the kind of
// mismatch that reads as correct in both the doc and the code.
func (mi *motifIndex) CarrierTitles(ctx context.Context, branch, clusterKey string, limit int) ([]string, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("CarrierTitles: %w", err)
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx, `
		SELECT f.title,
		       MAX(COALESCE(cl.committed_at, 0)) AS cl_committed_at,
		       MAX(f.id) AS newest_id
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		  JOIN fact_motifs m ON m.fact_id = bf.fact_id
		  LEFT JOIN motif_aliases a ON a.branch_id = bf.branch_id AND a.motif = m.motif
		  LEFT JOIN commit_log cl ON cl.commit_hash = bf.commit_hash AND cl.path = bf.path
		 WHERE bf.branch_id = ?
		   AND COALESCE(NULLIF(a.cluster_key, ''), knomit_motif_key(m.motif)) = ?
		 GROUP BY f.title
		 -- fact id breaks the timestamp tie. commit_log timestamps are
		 -- second-granularity, so a burst of writes — a bulk import, an agent
		 -- session, or a test fixture — lands them all on the same second and
		 -- the sort silently falls back to whatever comes next. Facts rows are
		 -- append-only and content-addressed, so a higher id is strictly later.
		 ORDER BY cl_committed_at DESC, newest_id DESC, f.title ASC
		 LIMIT ?`, branchID, clusterKey, limit)
	if err != nil {
		return nil, fmt.Errorf("CarrierTitles: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		var committedAt, newestID int64
		if err := rows.Scan(&t, &committedAt, &newestID); err != nil {
			return nil, fmt.Errorf("CarrierTitles: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
