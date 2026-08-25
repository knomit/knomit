package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Blind definitions, one per motif cluster (blueprint §3.2).
//
// Everything here keys on cluster_key, never on canonical_id. The
// representative spelling flips as usage shifts; the cluster it names does not,
// and a definition keyed to the representative would be orphaned by a change
// that meant nothing (designer rider 2026-08-21).

// DefinitionTarget is one cluster the definition pass should author for, with
// the name it will be shown.
//
// Name is the ONLY corpus content that reaches the definition prompt. Carriers
// are deliberately absent: a writer who never saw them cannot name the systems
// they are about, which is what makes the generic register achievable rather
// than merely requested.
type DefinitionTarget struct {
	ClusterKey string
	Name       string
	// Members is the cluster's membership AT THE MOMENT THIS TARGET WAS
	// SELECTED. It travels with the target and is what PutDefinition stamps.
	//
	// Reading membership at write time instead was a race with a real window:
	// the define payload is built during Plan, and a judge merge applied later
	// in the SAME session changes the cluster before the definition comes back.
	// The definition — authored for the pre-merge cluster — would then be
	// stamped with post-merge membership, marked current, and never refreshed.
	// The staleness comparison would have been defeated for precisely the merge
	// case it was built to catch.
	Members string
	// Interim is the definition currently standing for this cluster, if any.
	// Non-empty means the cluster HAS a usable sentence and is queued because
	// its membership moved — not because it has nothing.
	Interim string
}

// ClustersNeedingDefinition returns the live clusters whose definition is
// missing or was authored over a different membership.
//
// Staleness is a COMPARISON, not a flag. Nothing has to remember to mark a
// cluster dirty, and nothing can forget: a judge merge, a spelling joining
// mechanically, and a member retiring all move membership, and all three should
// prompt a fresh sentence. A flag set by the merge path would have caught only
// the first.
func (mi *motifIndex) ClustersNeedingDefinition(ctx context.Context, branch string) ([]DefinitionTarget, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("ClustersNeedingDefinition: %w", err)
	}
	membership, err := mi.clusterMembership(ctx, branchID)
	if err != nil {
		return nil, err
	}
	clusters, err := mi.Clusters(ctx, branch)
	if err != nil {
		return nil, err
	}
	stored, err := mi.definitionRows(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// Ordered by the Clusters ordering (most frequent first), so a bounded
	// pass spends its budget on the vocabulary the corpus actually leans on.
	var out []DefinitionTarget
	for _, c := range clusters {
		row, defined := stored[c.ClusterKey]
		if defined && row.members == membership[c.ClusterKey] {
			continue
		}
		out = append(out, DefinitionTarget{
			ClusterKey: c.ClusterKey,
			Name:       c.CanonicalID,
			Members:    membership[c.ClusterKey],
			Interim:    row.definition, // empty when never defined
		})
	}
	return out, nil
}

type definitionRow struct {
	definition string
	members    string
}

func (mi *motifIndex) definitionRows(ctx context.Context, branchID int64) (map[string]definitionRow, error) {
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
		`SELECT cluster_key, definition, members FROM motif_definitions WHERE branch_id = ?`,
		branchID)
	if err != nil {
		return nil, fmt.Errorf("definitionRows: %w", err)
	}
	defer rows.Close()
	out := map[string]definitionRow{}
	for rows.Next() {
		var key string
		var r definitionRow
		if err := rows.Scan(&key, &r.definition, &r.members); err != nil {
			return nil, fmt.Errorf("definitionRows: scan: %w", err)
		}
		out[key] = r
	}
	return out, rows.Err()
}

// DefinitionStamp is the membership a definition is recorded against.
//
// A STRUCT rather than a bare string because the two cases an empty string was
// being asked to cover are genuinely different, and conflating them silently
// disabled the protection this stamp exists to provide. "The caller carried a
// membership and it happens to be empty" is an unresolved cluster — real, and
// exactly the state a corpus is in when the alias rebuild has not run or has
// failed. "The caller has no membership" is a definition written outside a
// pass. The first must be stamped as given; only the second may fall back to
// reading current membership, which is the read-at-write-time behaviour the
// stamp was introduced to remove.
type DefinitionStamp struct {
	Members string
	// Known says the Members field is the caller's answer, empty or not. False
	// means "I have none; read the current membership" — the zero value, so a
	// caller that has not thought about it gets the old, safe-for-them
	// behaviour rather than an accidental empty stamp.
	Known bool
}

// PutDefinition stores a cluster's definition, stamped with the membership it
// was AUTHORED AGAINST.
//
// The membership is supplied by the caller — carried from the DefinitionTarget
// the authoring pass was given — rather than read here. Reading it here would
// stamp whatever the cluster looks like NOW, and a judge merge applied between
// planning and applying would mark a pre-merge definition as current for a
// post-merge cluster, permanently: nothing would ever re-queue it.
func (mi *motifIndex) PutDefinition(ctx context.Context, branch, clusterKey, definition string, stamp DefinitionStamp) error {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("PutDefinition: %w", err)
	}
	members := stamp.Members
	if !stamp.Known {
		current, merr := mi.clusterMembership(ctx, branchID)
		if merr != nil {
			return merr
		}
		members = current[clusterKey]
	}
	if _, err := conn(ctx, mi.rh.db).ExecContext(ctx,
		`INSERT INTO motif_definitions(branch_id, cluster_key, definition, members, authored_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(branch_id, cluster_key) DO UPDATE SET
		     definition  = excluded.definition,
		     members     = excluded.members,
		     authored_at = excluded.authored_at`,
		branchID, clusterKey, definition, members,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("PutDefinition: %w", err)
	}
	return nil
}

// Definition returns a cluster's standing definition, if it has one.
//
// Returns a STALE definition rather than nothing (designer ruling): a judge
// merge asserts the phrasings name the same mechanism, so the survivor's
// sentence is approximately right for the union, and gapping the cluster is
// worse than a slightly wide sentence. ClustersNeedingDefinition is what
// queues it for refresh.
func (mi *motifIndex) Definition(ctx context.Context, branch, clusterKey string) (string, bool, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return "", false, fmt.Errorf("Definition: %w", err)
	}
	var def string
	err = conn(ctx, mi.rh.db).QueryRowContext(ctx,
		`SELECT definition FROM motif_definitions WHERE branch_id = ? AND cluster_key = ?`,
		branchID, clusterKey).Scan(&def)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("Definition: %w", err)
	}
	return def, true, nil
}

// MotifVocabularyHealth is the §3.3 health picture of a corpus's motif
// vocabulary. Diagnostic only — nothing branches on it (MN6).
type MotifVocabularyHealth struct {
	// Clusters is the size of the resolved vocabulary.
	Clusters int
	// Recurring is how many clusters have df >= 2 — the ones that actually
	// connect two facts. Recurrence is the kill-switch signal: a vocabulary
	// where this stays near zero is one where every write mints a name nothing
	// ever reuses, and the axis is dead (§3.4).
	Recurring int
	// Mints is one per cluster: every cluster was minted exactly once, by
	// whichever fact first used it.
	Mints int
	// Links is every SUBSEQUENT use — total motif instances minus the mints.
	//
	// Distinct from Recurring, and the difference is the point: Recurring
	// counts CLUSTERS that recur, Links counts USES that reused. A corpus with
	// one heavily-shared motif and fifty hapax has low recurrence and high
	// links; one where every cluster has exactly two carriers has high
	// recurrence and modest links. Both numbers are needed to tell those apart.
	Links int
}

// RecurrenceRate is the fraction of clusters carried by more than one fact.
func (h MotifVocabularyHealth) RecurrenceRate() float64 {
	if h.Clusters == 0 {
		return 0
	}
	return float64(h.Recurring) / float64(h.Clusters)
}

// MintToLinkRatio is mints per link. Above 1 means the vocabulary is growing
// faster than it is being reused.
func (h MotifVocabularyHealth) MintToLinkRatio() float64 {
	if h.Links == 0 {
		// No reuse at all. Reported as the mint count rather than as an
		// infinity or a zero: both of those read as "nothing to see", and this
		// is precisely the state §3.4's kill switch is watching for.
		return float64(h.Mints)
	}
	return float64(h.Mints) / float64(h.Links)
}

// VocabularyHealth computes §3.3's metrics over AUTHORED facts only.
//
// Authored-only is load-bearing, not a refinement. Distilled and discovered
// facts inherit or are prompted toward motifs the corpus already holds, so
// counting them would report the engine's own carry-over as evidence that
// humans are converging on a shared vocabulary — the metric would measure the
// mechanism instead of the thing the mechanism exists to detect, and would
// climb most on exactly the corpora where the axis is doing least.
func (mi *motifIndex) VocabularyHealth(ctx context.Context, branch string) (MotifVocabularyHealth, error) {
	var h MotifVocabularyHealth
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return h, fmt.Errorf("VocabularyHealth: %w", err)
	}
	// df per CLUSTER over authored facts: distinct carrier paths, so a fact
	// using two spellings of one mechanism counts once — the same rule TokenDF
	// and Clusters apply.
	// The SAME key expression Clusters uses, including the final fallback to the
	// spelling itself: a motif whose every token normalizes away has no
	// mechanical cluster to join, and letting them all share the empty key would
	// report one cluster where the point readers see several singletons. The two
	// queries agree because they compute the same thing, not because they were
	// written on the same day.
	key := motifClusterKeyExpr("m.motif")
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx, `
		SELECT `+key+`, COUNT(DISTINCT bf.path)
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		  JOIN fact_motifs m ON m.fact_id = bf.fact_id
		  LEFT JOIN motif_aliases a ON a.branch_id = bf.branch_id AND a.motif = m.motif
		 WHERE bf.branch_id = ? AND f.origin = 'authored'
		 GROUP BY `+key, branchID)
	if err != nil {
		return h, fmt.Errorf("VocabularyHealth: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var df int
		if err := rows.Scan(&key, &df); err != nil {
			return h, fmt.Errorf("VocabularyHealth: scan: %w", err)
		}
		h.Clusters++
		h.Mints++ // one mint per cluster, by whichever fact first used it
		if df >= 2 {
			h.Recurring++
		}
		h.Links += df - 1 // every use after the first
	}
	return h, rows.Err()
}

// BackfillTarget is one fact the backfill pass may offer motifs for.
//
// Domain and Entities ride along because the §11 subtraction residue is
// computed against them — title tokens MINUS subject tokens. Reading them from
// the indexed columns rather than re-parsing the blob keeps the residue
// computed over the same values every other subject-aware path uses.
type BackfillTarget struct {
	FactID   int64
	Path     string
	Title    string
	Domain   []string
	Entities []string
}

// LiveFactsWithoutMotifs returns AUTHORED live facts carrying no motifs,
// oldest fact id first, for the backfill pass.
//
// Authored-only, for the same reason the health metrics are: a distilled or
// discovered fact without motifs is the pipeline having decided it needed none,
// and re-asking would be the engine second-guessing itself rather than filling
// a gap a human left.
//
// Oldest-first is deterministic and gives a corpus a stable sweep order across
// sessions — a bounded pass that started somewhere different each time would
// re-offer the same facts and never reach the tail.
//
// This is the BACKLOG, and the backlog is what makes a session non-empty:
// facts that have never been judged. Two ways out of it, and they are
// different answers rather than one answer twice — a fact that GAINED a motif
// leaves via fact_motifs, and a fact an agent judged to carry none leaves via
// motif_backfill_judged. Without the second, "answered, none apply" was
// indistinguishable from "not yet asked", and such a fact was re-offered every
// session forever; on a corpus with enough of them they hold every slot and the
// sweep never reaches the tail.
//
// The judged record is keyed on fact_id, which is content-addressed: an edited
// fact is a new immutable row that has never been judged and correctly returns
// here. Level-triggered throughout — "backlog" is this comparison, not state
// anyone maintains.
func (mi *motifIndex) LiveFactsWithoutMotifs(ctx context.Context, branch string, limit int) ([]BackfillTarget, error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("LiveFactsWithoutMotifs: %w", err)
	}
	rows, err := conn(ctx, mi.rh.db).QueryContext(ctx, `
		SELECT f.id, bf.path, f.title, f.domain, f.entities
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ?
		   AND f.origin = 'authored'
		   AND NOT EXISTS (SELECT 1 FROM fact_motifs m WHERE m.fact_id = f.id)
		   AND NOT EXISTS (SELECT 1 FROM motif_backfill_judged j
		                    WHERE j.branch_id = bf.branch_id AND j.fact_id = f.id)
		 ORDER BY f.id ASC
		 LIMIT ?`, branchID, limit)
	if err != nil {
		return nil, fmt.Errorf("LiveFactsWithoutMotifs: %w", err)
	}
	defer rows.Close()
	var out []BackfillTarget
	for rows.Next() {
		var t BackfillTarget
		var domainJSON, entitiesJSON string
		if err := rows.Scan(&t.FactID, &t.Path, &t.Title, &domainJSON, &entitiesJSON); err != nil {
			return nil, fmt.Errorf("LiveFactsWithoutMotifs: scan: %w", err)
		}
		var refs []string
		logFactJSONUnmarshal("LiveFactsWithoutMotifs", t.Path, domainJSON, entitiesJSON, "null",
			&t.Domain, &t.Entities, &refs)
		out = append(out, t)
	}
	return out, rows.Err()
}

// MotifCoverage reports how many live AUTHORED facts carry at least one motif.
// Reported in health; nothing branches on it.
func (mi *motifIndex) MotifCoverage(ctx context.Context, branch string) (with, backlog, total int, err error) {
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MotifCoverage: %w", err)
	}
	// All three counts come from ONE query over ONE denominator, and the
	// backlog term repeats LiveFactsWithoutMotifs' predicate exactly
	// (knomit#124). The repetition is deliberate and it is the point: the
	// backlog reported to an operator must be the same population the offer
	// pool walks, or drain progress describes a queue nobody is draining.
	// Computing it in a second method with its own WHERE clause is how those
	// two drift apart.
	//
	// The three are a PARTITION — covered + declined + backlog = total — which
	// is what lets the caller derive `declined` without a fourth count that
	// could disagree with the other three.
	err = conn(ctx, mi.rh.db).QueryRowContext(ctx, `
		SELECT
		  COUNT(DISTINCT CASE WHEN EXISTS (
		      SELECT 1 FROM fact_motifs m WHERE m.fact_id = f.id) THEN bf.path END),
		  COUNT(DISTINCT CASE WHEN NOT EXISTS (
		      SELECT 1 FROM fact_motifs m WHERE m.fact_id = f.id)
		    AND NOT EXISTS (
		      SELECT 1 FROM motif_backfill_judged j
		       WHERE j.branch_id = bf.branch_id AND j.fact_id = f.id) THEN bf.path END),
		  COUNT(DISTINCT bf.path)
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = ? AND f.origin = 'authored'`, branchID).Scan(&with, &backlog, &total)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("MotifCoverage: %w", err)
	}
	return with, backlog, total, nil
}

// RecordBackfillJudgedEmpty records that the backfill pass asked about these
// facts and the answer was "no regularity here".
//
// ONLY that answer. A motif the write gate REFUSED is not this: the agent found
// a regularity and named it, and the name failed a shape rule. Recording that
// as judged would bury the fact with no motif and no trace of why, which is the
// opposite of what should happen — it must come back so the naming can be
// fixed. Silence about an offered fact is likewise not an answer about it.
//
// Idempotent: re-judging the same version is the same answer.
//
// Takes FACT IDS — the versions the agent was actually shown — and never
// resolves a path here. Resolving the branch's live pointer at write time was
// exactly wrong: the pass offers a fact, the agent answers minutes later, and
// an ordinary learn/update in between makes the live pointer a DIFFERENT
// version. Stamping that one records a verdict against content nobody read,
// and because the stamp is what removes a fact from the backlog, the new claim
// then goes permanently unjudged. The caller owns the binding (see
// LiveFactIDs) because the caller is the only layer that knows what it offered.
func (mi *motifIndex) RecordBackfillJudgedEmpty(ctx context.Context, branch string, factIDs []int64) error {
	if len(factIDs) == 0 {
		return nil
	}
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("RecordBackfillJudgedEmpty: %w", err)
	}
	now := time.Now().Unix()
	for _, id := range factIDs {
		if _, err := conn(ctx, mi.rh.db).ExecContext(ctx, `
			INSERT INTO motif_backfill_judged (branch_id, fact_id, judged_at)
			VALUES (?, ?, ?)
			ON CONFLICT(branch_id, fact_id) DO NOTHING`, branchID, id, now); err != nil {
			return fmt.Errorf("RecordBackfillJudgedEmpty: %w", err)
		}
	}
	return nil
}

// LiveFactIDs resolves paths to the fact ids this branch currently points at.
//
// A path missing from the result is a path the branch no longer carries; the
// caller must treat that as "not the version I was handed" rather than as an
// error, since a fact can legitimately be retracted mid-session.
//
// Not AbstractionIndex.FactIDsByPath: that query carries an
// `f.kind = 'epistemic'` filter it needs for the title-vector work and backfill
// does not want. Reusing it would make every pragmatic fact look absent, and a
// staleness guard reading "absent" would skip facts that are present and
// current — the quiet half of a wrong answer.
func (mi *motifIndex) LiveFactIDs(ctx context.Context, branch string, paths []string) (map[string]int64, error) {
	out := make(map[string]int64, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	branchID, err := mi.rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("LiveFactIDs: %w", err)
	}
	for start := 0; start < len(paths); start += sqlIDChunk {
		chunk := paths[start:min(start+sqlIDChunk, len(paths))]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, branchID)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := conn(ctx, mi.rh.db).QueryContext(ctx,
			`SELECT path, fact_id FROM branch_facts
			  WHERE branch_id = ? AND path IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("LiveFactIDs: %w", err)
		}
		for rows.Next() {
			var path string
			var id int64
			if err := rows.Scan(&path, &id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("LiveFactIDs: scan: %w", err)
			}
			out[path] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("LiveFactIDs: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
