package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
)

// discoverCommit matches the provenance line every discovered fact is written
// under (`internal/synthesize/discovery.go`).
//
// THE COMMIT MESSAGE IS THE INSTRUMENT, and that is a choice worth stating.
// The obvious alternative is the discover work-item table, which records the
// bridge kind and lane directly — but work items live in the SESSIONS database,
// which does not travel with a corpus copy and is pruned in the ordinary course
// of running. The commit log travels with the repo, is append-only, and is the
// only record that survives long enough to answer a question about survival.
//
// Lesson 9 applies squarely: "did this fact survive?" is a temporal question,
// and the live index answers "is it here now" while saying nothing about what
// was ever written. Counting only live discovered facts would report 100%
// survival on every corpus, by construction.
var discoverCommit = regexp.MustCompile(`^discover-(forward|backward): emergent fact via bridge "(.+)"$`)

// survival reports how many discovered facts each bridge axis produced and how
// many are still live — blueprint §8's "survival after N review/harden sessions
// >= the token-bridge baseline".
func survival(ctx context.Context, corpus, scratch string) error {
	svc, _, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	// The vocabulary decides the axis: a motif bridge's token is a canonical
	// motif id, an entity/domain bridge's is a tag. Anything the alias table
	// knows is a motif.
	table, err := svc.Motifs().AliasTable(ctx, branch)
	if err != nil {
		return fmt.Errorf("alias table: %w", err)
	}
	isMotif := map[string]bool{}
	for spelling, canon := range table {
		isMotif[spelling] = true
		isMotif[canon] = true
	}

	// RAW READ OF commit_log, DISCLOSED. The store exposes no "walk every
	// commit" API — its history surface is per-path or per-commit-hash, and
	// this question is neither. Adding one to internal/store for a measurement
	// tool would be product surface grown for a report, so the tool reads the
	// table directly, read-only, on a lab COPY.
	//
	// This is NOT the habit the annex's §11.2 warned about. That was using a
	// live-index read to answer a temporal question; here the commit log IS the
	// temporal record and the live index is what would give the wrong answer.
	db, err := openReadOnly(copyPath(scratch, corpus))
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := discoverCommits(ctx, db)
	if err != nil {
		return fmt.Errorf("commit log: %w", err)
	}

	type arm struct{ Written, Live int }
	arms := map[string]*arm{"motif": {}, "entity-domain": {}}
	type row struct {
		Path, Token, Direction, Axis string
		Live                         bool
	}
	var detail []row

	// ONE ROW PER FACT, not per commit. commit_log is keyed (commit_hash,
	// path), and a fact can be touched by more than one discover-provenance
	// commit — its write, then a later edit under the same message. Counting
	// rows would report "4 discovered facts" for two, which is the unit
	// silently not matching the label.
	seenPath := map[string]bool{}
	for _, c := range rows {
		m := discoverCommit.FindStringSubmatch(c.Message)
		if m == nil {
			continue
		}
		if seenPath[c.Path] {
			continue
		}
		seenPath[c.Path] = true
		axis := "entity-domain"
		if isMotif[m[2]] {
			axis = "motif"
		}
		live := false
		if _, err := svc.FactQuery().GetByPath(ctx, branch, c.Path); err == nil {
			live = true
		}
		arms[axis].Written++
		if live {
			arms[axis].Live++
		}
		detail = append(detail, row{Path: c.Path, Token: m[2], Direction: m[1], Axis: axis, Live: live})
	}
	sort.Slice(detail, func(i, j int) bool { return detail[i].Path < detail[j].Path })

	out := map[string]any{
		"corpus": corpus,
		"branch": branch,
		"instrument": "commit-log walk for the discover provenance line, joined to branch_facts for " +
			"liveness. NOT a live-index scan: counting only live discovered facts would report 100% " +
			"survival on every corpus by construction (lesson 9).",
		"attribution": "a bridge token the alias table knows is a motif; anything else is entity/domain. " +
			"A token used by BOTH axes would be misattributed to motif — measured incidence below.",
		"unit": "one row per distinct FACT PATH written under a discover provenance line, " +
			"not per commit — a fact touched by two such commits counts once.",
		"detail": detail,
	}
	for name, a := range arms {
		e := map[string]any{"written": a.Written, "live": a.Live}
		if a.Written > 0 {
			e["survival"] = round4(float64(a.Live) / float64(a.Written))
		}
		out[name] = e
	}
	// The attribution's own failure mode, counted rather than assumed away.
	collisions := 0
	for _, d := range detail {
		if d.Axis == "motif" && !isMotif[d.Token] {
			collisions++
		}
	}
	out["ambiguous_tokens"] = collisions

	fmt.Fprintf(os.Stderr, "%s: motif %d/%d live, entity-domain %d/%d live\n", corpus,
		arms["motif"].Live, arms["motif"].Written,
		arms["entity-domain"].Live, arms["entity-domain"].Written)
	return emit(out)
}

type commitRow struct {
	Path    string
	Message string
}

// openReadOnly opens the lab copy for the commit-log walk. The lab guard has
// already run in open(); this is the same file, opened again for reading only.
func openReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+path+"?mode=ro")
}

func discoverCommits(ctx context.Context, db *sql.DB) ([]commitRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT path, message FROM commit_log WHERE message LIKE 'discover-%' ORDER BY committed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commitRow
	for rows.Next() {
		var c commitRow
		if err := rows.Scan(&c.Path, &c.Message); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
