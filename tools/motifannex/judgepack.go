package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// judgepack combines the primary and supplementary packs into ONE blind pack
// with a UNIFORM id space.
//
// Review finding M-1: the first run interleaved `H###` and `S###` ids, and the
// prefix was a perfect membership oracle for the supplementary arm — all 12
// `S###` were MOTIF-FIXTURE and nothing else was. The record claimed "ids carry
// no arm information" and the judge's own reply disproved it, characterising
// "the S-pairs" as a group. A judge that has identified twelve pairs as one
// homogeneous set can drift across them in either direction, whatever its
// intent.
//
// Uniform `P###` ids, ordered by a hash of the pair's own paths so neither the
// source file nor the arm survives into the ordering. The two populations stay
// separate in the KEY, which is what rulings-6 requires — never pooled in
// analysis, only in the pack the judge reads.
func judgepack(dir string) error {
	type packItem struct {
		ID     string `json:"id"`
		ATitle string `json:"a_title"`
		ABody  string `json:"a_body"`
		BTitle string `json:"b_title"`
		BBody  string `json:"b_body"`
	}
	type keyItem struct {
		ID         string `json:"id"`
		Arm        string `json:"arm"`
		Population string `json:"population"`
		Corpus     string `json:"corpus,omitempty"`
		Token      string `json:"token,omitempty"`
		A          string `json:"a"`
		B          string `json:"b"`
	}

	var primary struct {
		Pack []packItem `json:"pack"`
		Key  []keyItem  `json:"key"`
	}
	var supp struct {
		Pack []packItem `json:"pack"`
		Key  []keyItem  `json:"key"`
	}
	if err := readJSON(filepath.Join(dir, "primary.json"), &primary); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(dir, "supplementary.json"), &supp); err != nil {
		return err
	}

	type row struct {
		item packItem
		key  keyItem
	}
	var rows []row
	add := func(pack []packItem, key []keyItem, population string) error {
		if len(pack) != len(key) {
			return fmt.Errorf("%s: pack has %d items and key has %d", population, len(pack), len(key))
		}
		byID := map[string]keyItem{}
		for _, k := range key {
			byID[k.ID] = k
		}
		for _, p := range pack {
			k, ok := byID[p.ID]
			if !ok {
				return fmt.Errorf("%s: pack item %s has no key row", population, p.ID)
			}
			k.Population = population
			rows = append(rows, row{item: p, key: k})
		}
		return nil
	}
	if err := add(primary.Pack, primary.Key, "primary"); err != nil {
		return err
	}
	if err := add(supp.Pack, supp.Key, "supplementary"); err != nil {
		return err
	}

	// Order by a hash of the PAIR's paths: stable across runs, and carrying
	// nothing about which file a pair arrived in or which arm it belongs to.
	sort.Slice(rows, func(i, j int) bool {
		return fnv(rows[i].key.A+rows[i].key.B) < fnv(rows[j].key.A+rows[j].key.B)
	})

	var pack []packItem
	var key []keyItem
	md := []string{"# Fact pair pack", "", "For each pair, give one verdict. Output JSON only.", ""}
	for i := range rows {
		id := fmt.Sprintf("P%03d", i+1)
		rows[i].item.ID, rows[i].key.ID = id, id
		pack = append(pack, rows[i].item)
		key = append(key, rows[i].key)
		md = append(md,
			"## "+id,
			"FACT 1: "+rows[i].item.ATitle, "  "+rows[i].item.ABody,
			"FACT 2: "+rows[i].item.BTitle, "  "+rows[i].item.BBody, "")
	}

	// The judge must never see the key, so it is written separately and the
	// pack is the only file handed over.
	if err := os.WriteFile(filepath.Join(dir, "judge_pack.md"), []byte(join(md)), 0o644); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "combined.key.json"), key); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, k := range key {
		counts[k.Arm]++
	}
	fmt.Fprintf(os.Stderr, "combined %d pairs, uniform ids; arms=%v\n", len(pack), counts)
	return emit(map[string]any{"pairs": len(pack), "arms": counts})
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func join(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

var _ = context.Background
