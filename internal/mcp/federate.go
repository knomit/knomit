package mcp

// Federation helpers for lens read fan-out (lenses RFC §7): reciprocal rank
// fusion for relevance, k-way timestamp merge for recency, the kb:// wire
// path form, and ontology-aware fan-out target selection. Everything here is
// pure — no store access — so it is exhaustively unit-testable.

import (
	"fmt"
	"sort"
	"strings"

	"knomit/internal/repos"
)

// rrfK is the reciprocal-rank-fusion constant: score = 1/(rrfK + rank).
const rrfK = 60

// repoIDWireLen is how many hex chars of the root-commit hash appear in the
// kb://<id>/ wire form (RFC §6.1).
const repoIDWireLen = 12

const kbScheme = "kb://"

// mountRef addresses one row of a per-mount result list: lists[Mount][Rank].
type mountRef struct {
	Mount int
	Rank  int
}

// fuseRRF orders the union of per-mount ranked lists by reciprocal rank
// fusion (RFC §7.1). Replica mounts are rejected at lens create, so every
// fact appears in exactly one list and the fused score collapses to the
// single term 1/(rrfK+rank). Equal fused scores (same rank, different
// mounts) tie-break by mount order so fusion is deterministic; with one
// list the output order is the input order (the N=1 no-behavior-change
// invariant).
func fuseRRF(listLens []int) []mountRef {
	var out []mountRef
	for m, n := range listLens {
		for r := range n {
			out = append(out, mountRef{Mount: m, Rank: r})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		si := 1.0 / float64(rrfK+out[i].Rank)
		sj := 1.0 / float64(rrfK+out[j].Rank)
		if si != sj {
			return si > sj
		}
		return out[i].Mount < out[j].Mount
	})
	return out
}

// mergeRecent orders the union of per-mount recency lists by committed_at
// DESC, capped at max. Commit timestamps are directly comparable across
// mounts, so this is a plain k-way timestamp merge — RRF would be wrong
// here (rank fusion exists for INcomparable relevance scores; RFC §7.1).
// Ties break by mount order then per-mount position, deterministically.
// Each input list must already be committed_at-DESC — the order RecentFacts
// returns for a text-LESS recency query. WITH a text query RecentFacts returns
// relevance order instead, which federates by fuseRRF, not this merge (see
// queryRecent).
func mergeRecent(stamps [][]int64, max int) []mountRef {
	var out []mountRef
	for m, list := range stamps {
		for r := range list {
			out = append(out, mountRef{Mount: m, Rank: r})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := stamps[out[i].Mount][out[i].Rank], stamps[out[j].Mount][out[j].Rank]
		if a != b {
			return a > b
		}
		if out[i].Mount != out[j].Mount {
			return out[i].Mount < out[j].Mount
		}
		return out[i].Rank < out[j].Rank
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// id12 shortens a full root-commit hash to the wire form.
func id12(fullID string) string {
	if len(fullID) <= repoIDWireLen {
		return fullID
	}
	return fullID[:repoIDWireLen]
}

// qualifyPath renders the canonical qualified wire form (RFC §6.2).
func qualifyPath(id12, rel string) string {
	return kbScheme + id12 + "/" + rel
}

// parseQualifiedPath splits a wire path. A bare path returns qualified=false
// with rel=p. A kb:// path must carry exactly repoIDWireLen lowercase-hex id
// chars and a non-empty repo-relative remainder; anything else is malformed.
func parseQualifiedPath(p string) (id, rel string, qualified bool, err error) {
	rest, ok := strings.CutPrefix(p, kbScheme)
	if !ok {
		return "", p, false, nil
	}
	id, rel, found := strings.Cut(rest, "/")
	if !found || rel == "" || len(id) != repoIDWireLen || !isLowerHex(id) {
		return "", "", true, fmt.Errorf("malformed kb:// path %q — want kb://<12-hex-repo-id>/<path>", p)
	}
	return id, rel, true, nil
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// topicOfPathFilter extracts the topic constraint from an unqualified path
// filter, for ontology-aware fan-out (RFC §7.2). The path filter is a PREFIX
// match, so a topic is only constrained when the filter delimits a complete
// topic segment ("kb/<topic>/…"): "kb/decisions/" constrains to decisions,
// but "kb/decisions" is a prefix that could also match "kb/decisions-x/",
// so it constrains nothing.
func topicOfPathFilter(pathFilter string) string {
	rest, ok := strings.CutPrefix(pathFilter, "kb/")
	if !ok {
		return ""
	}
	topic, _, found := strings.Cut(rest, "/")
	if !found || topic == "" {
		return ""
	}
	return topic
}

// fanTarget is one mount a query fans out to, with its per-mount
// (repo-relative) path filter.
type fanTarget struct {
	RT   repos.ReadTarget
	Path string
}

// readTargetsFor selects a query's fan-out targets (RFC §6.2 addressing +
// §7.2 ontology-aware fan-out). A kb://-qualified path filter restricts the
// query to that single mount with the filter made repo-relative; an
// unqualified filter applies per-mount as-is, skipping mounts whose ontology
// lacks a fully-delimited topic constraint. The skip is a pure internal
// optimization: a skipped mount is indistinguishable from one that matched
// nothing (decision 17 — no coverage metadata, ever).
func readTargetsFor(b *repos.Binding, pathFilter string) ([]fanTarget, error) {
	id, rel, qualified, err := parseQualifiedPath(pathFilter)
	if err != nil {
		return nil, err
	}
	if qualified {
		rt, ok := b.ByID(id)
		if !ok {
			return nil, fmt.Errorf("repo %s is not mounted in this binding", id)
		}
		return []fanTarget{{RT: rt, Path: rel}}, nil
	}
	topic := topicOfPathFilter(rel)
	var out []fanTarget
	for _, rt := range b.Reads() {
		if topic != "" {
			if o := rt.RI.Ontology(); o != nil && !containsString(o.TopicNames(), topic) {
				continue
			}
		}
		out = append(out, fanTarget{RT: rt, Path: rel})
	}
	return out, nil
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
