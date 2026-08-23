// refprobe answers "does this ref resolve?" using knomit's OWN temporal
// predicate — store.FactQuery.FactExistsAt — rather than a raw index read.
//
// The distinction is the whole point. `SELECT ... FROM facts WHERE path = ?`
// asks whether a path is CURRENTLY INDEXED, which is what a retracted or
// superseded fact also looks like. FactExistsAt asks the question the reader
// and the write-time ref gate both ask: does the target have any version
// reachable from here, walking back past retractions.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
)

func main() {
	dbPath := os.Args[1]
	svc, err := store.Open(dbPath)
	must(err)
	defer svc.Close()
	must(svc.OpenRepo())
	ctx := context.Background()

	branches, err := svc.Branches().ListBranches(ctx)
	must(err)
	branch, best := "", -1
	for _, b := range branches {
		res, err := svc.FactQuery().Search(ctx, b.Name, store.SearchOptions{Limit: 100_000})
		if err != nil {
			continue
		}
		if len(res) > best {
			branch, best = b.Name, len(res)
		}
	}
	fmt.Printf("branch %q (%d facts)\n\n", branch, best)

	facts, err := svc.FactQuery().Search(ctx, branch, store.SearchOptions{Limit: 100_000})
	must(err)

	// "paths" mode: dump every live path, so a set comparison against an
	// external artefact does not need a raw index read.
	if len(os.Args) > 3 && os.Args[2] == "blast" {
		for _, path := range os.Args[3:] {
			r, err := svc.GraphStore().BlastRadius(ctx, branch, path)
			must(err)
			fmt.Printf("BLAST %d  %s\n", r, path)
		}
		return
	}
	if len(os.Args) > 3 && os.Args[2] == "show" {
		for _, path := range os.Args[3:] {
			rec, err := svc.Facts().ReadFact(ctx, branch, path, nil)
			must(err)
			pf, err := fact.ParseFact(path, rec.Content)
			must(err)
			fmt.Printf("FILE %s\nTITLE %s\nTYPE %s\nDOMAIN %v\nENTITIES %v\nMOTIFS %v\nBODY %s\n---\n",
				path, pf.Title, pf.Type, pf.Domain, pf.Entities, pf.Motifs, pf.Body)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[2] == "paths" {
		for _, f := range facts {
			fmt.Println(f.Path)
		}
		return
	}

	type miss struct{ from, to string }
	var unresolved []miss
	targets := map[string]bool{}
	checked := map[string]bool{}
	nInternal, nForeign := 0, 0

	for _, f := range facts {
		rec, err := svc.Facts().ReadFact(ctx, branch, f.Path, nil)
		if err != nil {
			continue
		}
		parsed, err := fact.ParseFact(f.Path, rec.Content)
		if err != nil {
			continue
		}
		for _, r := range parsed.Refs {
			// localRepoID "" so kb://<id>/ classifies FOREIGN and only BARE
			// repo-relative paths come back local. Bare paths are the form the
			// dedup failure names, and they are what the local gate checks.
			c := fact.ClassifyRef(r, "")
			switch c.Kind {
			case fact.RefForeignFact:
				nForeign++
				continue
			case fact.RefLocalFact:
			default:
				continue
			}
			r = c.Path
			nInternal++
			if !checked[r] {
				ok, err := svc.FactQuery().FactExistsAt(ctx, branch, r, "")
				must(err)
				checked[r] = true
				targets[r] = ok
			}
			if !targets[r] {
				unresolved = append(unresolved, miss{f.Path, r})
			}
		}
	}

	nBad := 0
	for _, ok := range targets {
		if !ok {
			nBad++
		}
	}
	fmt.Printf("BARE local fact refs: %d (%d distinct targets)\nkb://<id>/ qualified refs (foreign, never locally checked): %d\n", nInternal, len(targets), nForeign)
	fmt.Printf("targets that DO resolve via FactExistsAt (incl. retracted/superseded): %d\n", len(targets)-nBad)
	fmt.Printf("targets that do NOT resolve: %d\n", nBad)
	srcs := map[string]bool{}
	for _, m := range unresolved {
		srcs[m.from] = true
	}
	fmt.Printf("facts carrying at least one unresolvable ref: %d\n\n", len(srcs))

	var list []miss
	list = append(list, unresolved...)
	sort.Slice(list, func(i, j int) bool { return list[i].to < list[j].to })
	for i, m := range list {
		if i >= 15 {
			fmt.Printf("... and %d more\n", len(list)-15)
			break
		}
		fmt.Printf("  %s\n    cites %s\n", m.from, m.to)
	}
	_ = json.Marshal
	_ = filepath.Join
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
