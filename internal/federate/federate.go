// Package federate holds the lens union-read machinery shared by every reader
// front-end (MCP handlers, REST endpoints): reciprocal rank fusion for
// relevance, k-way timestamp merge for recency, the kb:// wire path form, and
// ontology-aware fan-out target selection. Everything here is pure — no store
// access — so it is exhaustively unit-testable (lenses RFC §7).
package federate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"knomit/internal/repos"
)

// rrfK is the reciprocal-rank-fusion constant: score = 1/(rrfK + rank).
const rrfK = 60

// repoIDWireLen is how many hex chars of the root-commit hash appear in the
// kb://<id>/ wire form (RFC §6.1).
const repoIDWireLen = 12

// KBScheme is the kb:// wire-path scheme prefix (RFC §6.1).
const KBScheme = "kb://"

// MountRef addresses one row of a per-mount result list: lists[Mount][Rank].
type MountRef struct {
	Mount int
	Rank  int
}

// FuseRRF orders the union of per-mount ranked lists by reciprocal rank
// fusion (RFC §7.1). Replica mounts are rejected at lens create, so every
// fact appears in exactly one list and the fused score collapses to the
// single term 1/(rrfK+rank). Equal fused scores (same rank, different
// mounts) tie-break by mount order so fusion is deterministic; with one
// list the output order is the input order (the N=1 no-behavior-change
// invariant).
func FuseRRF(listLens []int) []MountRef {
	var out []MountRef
	for m, n := range listLens {
		for r := range n {
			out = append(out, MountRef{Mount: m, Rank: r})
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

// MergeRecent orders the union of per-mount recency lists by committed_at
// DESC, capped at max. Commit timestamps are directly comparable across
// mounts, so this is a plain k-way timestamp merge — RRF would be wrong
// here (rank fusion exists for INcomparable relevance scores; RFC §7.1).
// Ties break by mount order then per-mount position, deterministically.
// Each input list must already be committed_at-DESC — the order RecentFacts
// returns for a text-LESS recency query. WITH a text query RecentFacts returns
// relevance order instead, which federates by FuseRRF, not this merge (see
// queryRecent).
func MergeRecent(stamps [][]int64, max int) []MountRef {
	var out []MountRef
	for m, list := range stamps {
		for r := range list {
			out = append(out, MountRef{Mount: m, Rank: r})
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

// ReadSetFingerprint is the canonical identity of a binding's READ SET: every
// mount rendered as id12@len:branch, sorted lexicographically and comma-joined
// (lenses RFC §7.3). A cursor pins this fingerprint at mint; resume recomputes
// it from the current binding and rejects any mismatch. So re-pinning a mount to
// a different branch — or swapping the mount set — under the SAME binding name
// invalidates in-flight cursors instead of silently hydrating rows against the
// new read set. Sorting makes the fingerprint order-insensitive; a lens-of-one
// collapses to a single "id12@len:branch" term.
//
// id12 is fixed 12-hex (never '@' or ','), but a branch name is free-form and
// may contain the '@'/',' separators — so the branch is length-prefixed to keep
// the encoding INJECTIVE. Without it a single mount at branch "a,<id2>@b" would
// serialize identically to two mounts "<id1>@a" + "<id2>@b", colliding two
// distinct read sets and wrongly accepting a stale cursor after a lens
// redefinition (lenses RFC §7.3).
func ReadSetFingerprint(b *repos.Binding) string {
	reads := b.Reads()
	parts := make([]string, len(reads))
	for i, rt := range reads {
		parts[i] = ID12(rt.RI.ID()) + "@" + strconv.Itoa(len(rt.Branch)) + ":" + rt.Branch
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// ID12 shortens a full root-commit hash to the wire form.
func ID12(fullID string) string {
	if len(fullID) <= repoIDWireLen {
		return fullID
	}
	return fullID[:repoIDWireLen]
}

// QualifyPath renders the canonical qualified wire form (RFC §6.2).
func QualifyPath(id12, rel string) string {
	return KBScheme + id12 + "/" + rel
}

// ParseQualifiedPath splits a wire path. A bare path returns qualified=false
// with rel=p. A kb:// path must carry exactly repoIDWireLen lowercase-hex id
// chars and a non-empty repo-relative remainder; anything else is malformed.
func ParseQualifiedPath(p string) (id, rel string, qualified bool, err error) {
	rest, ok := strings.CutPrefix(p, KBScheme)
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

// WriteRepoPath resolves a write-tool file argument to the repo-relative path
// on the binding's write repo (RFC §6.2). Unqualified paths are the write
// repo's own ("current directory"); kb://<write-id>/… is accepted and exactly
// equivalent to bare; a qualified path to any OTHER mount is a read-only-mount
// error (writes have exactly one target), and an unmounted ID is the
// not-mounted error. The error naming the repo is prose, not addressing.
func WriteRepoPath(b *repos.Binding, file string) (string, error) {
	id, rel, qualified, err := ParseQualifiedPath(file)
	if err != nil {
		return "", err
	}
	if !qualified {
		return rel, nil
	}
	rt, ok := b.ByID(id)
	if !ok {
		return "", fmt.Errorf("repo %s is not mounted in this binding", id)
	}
	if rt.RI != b.Write() {
		return "", fmt.Errorf("read-only mount: repo %s is not this binding's write repo — facts there can only be changed through their own endpoint", id)
	}
	return rel, nil
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

// Target is one mount a query fans out to, with its per-mount
// (repo-relative) path filter.
type Target struct {
	RT   repos.ReadTarget
	Path string
}

// WriteFirstWinners computes the per-mount dedupe winners shared by EVERY lens
// union read — the web /facts, /search, and /topics handlers and the MCP
// knomit_query fan-out alike. Rows are deduped by repo-relative path (pathOf
// extracts it from each mount's element): the WRITE mount's copy always wins —
// its facts are the lens's editable, canonical rows — so it is recorded first;
// remaining collisions resolve in binding order. (Reads() is sorted by repo
// name, so the write mount is not positionally "first" in general; prioritise it
// explicitly.) The result maps a rel path to its winning target index; a caller
// emits a row only when its mount equals the winner, so a shadowed copy never
// appears even if it ranks higher.
//
// Collisions are real, not hypothetical: replica mounts (same repo ID) are
// rejected at lens create, but a re-rooted fork of a read-mounted upstream has a
// DIFFERENT root-commit ID (so it mounts) yet shares the upstream's
// server-generated fact UUIDs (so the same kb/<topic>/<cat>/<uuid>.md path
// appears on two mounts). Every union surface MUST agree on winners — hence one
// definition here, not one per consumer.
func WriteFirstWinners[T any](targets []Target, write *repos.RepoInstance, lists [][]T, pathOf func(T) string) map[string]int {
	winner := make(map[string]int)
	record := func(isWrite bool) {
		for i, t := range targets {
			if (t.RT.RI == write) != isWrite {
				continue
			}
			for _, e := range lists[i] {
				p := pathOf(e)
				if _, seen := winner[p]; !seen {
					winner[p] = i
				}
			}
		}
	}
	record(true)  // write mount first
	record(false) // then read mounts in binding order
	return winner
}

// ReadTargetsFor selects a query's fan-out targets (RFC §6.2 addressing +
// §7.2 ontology-aware fan-out). A kb://-qualified path filter restricts the
// query to that single mount with the filter made repo-relative; an
// unqualified filter applies per-mount as-is, skipping mounts whose ontology
// lacks a fully-delimited topic constraint. The skip is a pure internal
// optimization: a skipped mount is indistinguishable from one that matched
// nothing (decision 17 — no coverage metadata, ever).
func ReadTargetsFor(b *repos.Binding, pathFilter string) ([]Target, error) {
	id, rel, qualified, err := ParseQualifiedPath(pathFilter)
	if err != nil {
		return nil, err
	}
	if qualified {
		rt, ok := b.ByID(id)
		if !ok {
			return nil, fmt.Errorf("repo %s is not mounted in this binding", id)
		}
		return []Target{{RT: rt, Path: rel}}, nil
	}
	topic := topicOfPathFilter(rel)
	var out []Target
	for _, rt := range b.Reads() {
		if topic != "" {
			if o := rt.RI.Ontology(); o != nil && !containsString(o.TopicNames(), topic) {
				continue
			}
		}
		out = append(out, Target{RT: rt, Path: rel})
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
