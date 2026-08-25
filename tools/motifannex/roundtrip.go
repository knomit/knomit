package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"knomit/internal/fact"
)

// roundtrip is carried-forward register entry 12's READ-ONLY audit: for every
// live fact, parse the stored bytes and re-serialize them, and classify every
// difference.
//
// WHAT THE QUESTION IS. `ParseFact → mutate → SerializeFact → WriteFact` is a
// shape several write paths share — REINFORCE (remediated by the Phase-3 write
// contract), prune's merge, update. Where the round trip is not the identity,
// any of those paths silently rewrites bytes it was not asked to touch. The
// Phase-3 fidelity measurement found 66 core facts whose illegal stored
// type/origin pairing is reattributed to `authored` by a non-skipping rewrite;
// this re-measures on the current corpus state and looks for classes beyond it.
//
// NOTHING IS WRITTEN, and no live KB is opened — refuseLivePath makes that
// structural rather than a promise. The re-serialized bytes exist only in
// memory, long enough to be compared.
func roundtrip(ctx context.Context, corpus, scratch string) error {
	_, _, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	db, err := openReadOnly(copyPath(scratch, corpus))
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT bf.path, o.data
		  FROM branch_facts bf
		  JOIN facts f ON f.id = bf.fact_id
		  JOIN objects o ON o.hash = f.blob_hash
		 WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?)
		 ORDER BY bf.path`, branch)
	if err != nil {
		return fmt.Errorf("read stored blobs: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	examples := map[string][]string{}
	total, identical, unparsable := 0, 0, 0

	for rows.Next() {
		var path string
		var data []byte
		if err := rows.Scan(&path, &data); err != nil {
			return err
		}
		total++
		stored := string(data)

		f, perr := fact.ParseFact(path, stored)
		if perr != nil {
			unparsable++
			note(counts, examples, "unparsable", path)
			continue
		}
		out, serr := fact.SerializeFact(f)
		if serr != nil {
			note(counts, examples, "unserializable", path)
			continue
		}
		if out == stored {
			identical++
			// A fact whose motifs the parser dropped is NOT identical-safe just
			// because the bytes matched: check it anyway below.
		}

		if len(f.MotifWarnings) > 0 {
			note(counts, examples, "motif-loss", path)
		}
		if out == stored {
			continue
		}
		// RE-PARSE THE OUTPUT before classifying. Whether a changed origin line
		// is a semantic no-op or a silent reattribution cannot be read off the
		// bytes — it depends on what ParseFact makes of the NEW bytes. Asserting
		// it from the diff alone would be claiming a construction property
		// without reading the output (the annex's own §11 corollary).
		//
		// COMPARE AGAINST THE STORED BYTES, NOT THE PARSED STRUCT. The first
		// version compared reparsed.Origin to f.Origin — both of them ParseFact
		// output — and reported every one of these as a semantic no-op. It
		// cannot see the loss, because the loss happens AT parse: ParseFact
		// normalises an illegal `discovered observation` to `authored`, so both
		// sides of that comparison already say authored. This is the Phase-3
		// review's H1 finding committed a second time in the tool written to
		// measure it, and the fix is the same one: read the boundary the corpus
		// actually stores.
		reparsed, rerr := fact.ParseFact(path, out)
		storedOrigin := frontmatterValue(stored, "origin")
		lostOrigin := rerr == nil && storedOrigin != "" && string(reparsed.Origin) != storedOrigin
		for _, cls := range classifyDiff(stored, out, f) {
			if cls == "line-removed:origin" || cls == "origin-materialised-DIFFERENT" {
				if lostOrigin {
					cls = fmt.Sprintf("origin-REATTRIBUTED-%s-to-%s", storedOrigin, reparsed.Origin)
				} else {
					cls = "origin-line-changed-semantic-noop"
				}
			}
			note(counts, examples, cls, path)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	classes := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		classes = append(classes, map[string]any{
			"class": k, "facts": counts[k],
			"pct":      round4(float64(counts[k]) / float64(max(total, 1))),
			"examples": examples[k],
		})
	}

	fmt.Fprintf(os.Stderr, "%s: %d live facts, %d byte-identical, classes=%v\n",
		corpus, total, identical, counts)
	return emit(map[string]any{
		"corpus":     corpus,
		"branch":     branch,
		"instrument": "stored git blob -> fact.ParseFact -> fact.SerializeFact -> byte diff. READ-ONLY.",
		"population": "every live fact on the branch",
		"live_facts": total,
		"identical":  identical,
		"unparsable": unparsable,
		"classes":    classes,
		"motif_caveat": "the motif-loss column is meaningful for the FIRST time here, because these " +
			"copies carry motifs where the Phase-3 measurement's corpora carried none — and the motifs " +
			"it describes are ANNEX-WRITTEN, not production ones, so it is evidence about the parser " +
			"and not about any deployed corpus.",
	})
}

func note(counts map[string]int, examples map[string][]string, class, path string) {
	counts[class]++
	if len(examples[class]) < 5 {
		examples[class] = append(examples[class], path)
	}
}

// classifyDiff names every way the re-serialized bytes differ from the stored
// ones.
//
// Line-set based rather than positional: a reordered frontmatter key is not a
// value change, and reporting it as one would bury the classes that matter
// under noise. Every difference lands in a named bucket or in "other" —
// "other" is not a catch-all to be tidied away later, it is the bucket a class
// nobody anticipated shows up in, which is the reason to run this at all.
func classifyDiff(stored, out string, f fact.Fact) []string {
	var classes []string
	sLines := frontmatterLines(stored)
	oLines := frontmatterLines(out)

	added, removed := lineDelta(sLines, oLines)

	// A quote-only change appears TWICE in the delta — once as a removed line
	// and once as the requoted addition — so it has to be recognised on BOTH
	// sides or one change is reported as two classes. The first run did exactly
	// that: "line-added:refs 157" and "quote-style-only 157" were the same 157
	// facts, and the first name made a rendering difference look like a
	// structural one.
	for _, l := range added {
		key := lineKey(l)
		if quoteOnly(l, removed) {
			classes = append(classes, "quote-style-only")
			continue
		}
		switch {
		case key == "origin":
			if strings.TrimSpace(strings.TrimPrefix(l, "origin:")) == string(f.Origin) {
				classes = append(classes, "origin-materialised-to-parse-default")
			} else {
				classes = append(classes, "origin-materialised-DIFFERENT")
			}
		case key == "type" || key == "kind":
			classes = append(classes, "type-or-kind-rewritten")
		default:
			classes = append(classes, "line-added:"+key)
		}
	}
	for _, l := range removed {
		key := lineKey(l)
		if quoteOnly(l, added) {
			classes = append(classes, "quote-style-only")
			continue
		}
		classes = append(classes, "line-removed:"+key)
	}
	if len(classes) == 0 && stored != out {
		classes = append(classes, "other-body-or-whitespace")
	}
	return dedupeStrings(classes)
}

func frontmatterLines(s string) []string {
	parts := strings.SplitN(s, "---\n", 3)
	if len(parts) < 3 {
		return nil
	}
	return strings.Split(strings.TrimRight(parts[1], "\n"), "\n")
}

func lineKey(l string) string {
	if i := strings.Index(l, ":"); i > 0 {
		return strings.TrimSpace(l[:i])
	}
	return strings.TrimSpace(l)
}

// quoteOnly reports whether a removed line reappears among the added ones with
// only quote characters differing — YAML rendering, not a value change
// (rulings-5 clause 3).
func quoteOnly(removed string, added []string) bool {
	strip := func(s string) string {
		return strings.NewReplacer("'", "", `"`, "", " ", "").Replace(s)
	}
	for _, a := range added {
		if strip(a) == strip(removed) {
			return true
		}
	}
	return false
}

func lineDelta(before, after []string) (added, removed []string) {
	b := map[string]int{}
	for _, l := range before {
		b[l]++
	}
	a := map[string]int{}
	for _, l := range after {
		a[l]++
	}
	for l, n := range a {
		if b[l] < n {
			added = append(added, l)
		}
	}
	for l, n := range b {
		if a[l] < n {
			removed = append(removed, l)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var _ = sql.ErrNoRows

// frontmatterValue reads one frontmatter value out of the STORED bytes,
// deliberately without parsing.
//
// Every question about what a round trip LOSES has to be asked against the
// stored bytes: a parsed struct has already had the normalisation applied, so
// comparing one parse to another compares two copies of the same loss.
func frontmatterValue(stored, key string) string {
	for _, l := range frontmatterLines(stored) {
		if lineKey(l) == key {
			if i := strings.Index(l, ":"); i > 0 {
				return strings.TrimSpace(l[i+1:])
			}
		}
	}
	return ""
}
