package index

import "testing"

// localFactRefs keeps only the refs that can become DERIVED_FROM edges: facts
// in THIS repo, as repo-relative paths. A kb://<own-id>/… ref is the documented
// canonical form, so writing one must not silently produce nothing — which is
// what the old "anything not http(s) is a local candidate" filter did, by
// handing resolveTargetCommit the literal kb:// string as a path.
func TestLocalFactRefs(t *testing.T) {
	const local = "3ec012f5b4d2"
	in := []string{
		"kb/decisions/x/abc.md",                    // bare local
		"kb://3ec012f5b4d2/kb/invariants/y/def.md", // qualified SELF → local
		"kb://7b4887ce51d9/kb/invariants/z/ghi.md", // foreign → excluded
		"src://7b4887ce51d9/internal/x.go@aaa:bbb", // source → excluded
		"src://knomit/internal/legacy.go@ca1c272",  // legacy source → excluded
		"https://example.com/x",                    // external → excluded
		"file:///tmp/x.md",                         // external → excluded
		"kb://abc/kb/x.md",                         // malformed → excluded
	}
	want := []string{
		"kb/decisions/x/abc.md",
		"kb/invariants/y/def.md",
	}

	got := localFactRefs(in, local)
	if len(got) != len(want) {
		t.Fatalf("localFactRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("localFactRefs = %v, want %v", got, want)
		}
	}
}

// With no local repo id every kb:// ref reads as foreign, so self-qualified
// refs stop forming edges. That under-reports rather than inventing edges,
// which is the safe direction when identity is unresolvable.
func TestLocalFactRefs_EmptyRepoID(t *testing.T) {
	got := localFactRefs([]string{
		"kb/bare.md",
		"kb://3ec012f5b4d2/kb/qualified.md",
	}, "")
	if len(got) != 1 || got[0] != "kb/bare.md" {
		t.Fatalf("localFactRefs = %v, want [kb/bare.md]", got)
	}
}

// refSpecsOf is the ordered fact refs of a version (ref_edges): ord counts
// EVERY ref, so it equals json_each(facts.refs).key; local and cross-KB refs
// get a spec, the rest none; a (repo, path) cited twice keeps its first ord.
func TestRefSpecsOf(t *testing.T) {
	const local = "3ec012f5b4d2"
	got := refSpecsOf([]string{
		"https://example.com/x",                    // 0: external, no spec
		"kb://7b4887ce51d9/kb/invariants/z/ghi.md", // 1: cross-KB
		"kb/decisions/x/abc.md",                    // 2: local
		"src://7b4887ce51d9/internal/x.go@aaa:bbb", // 3: source, no spec
		"kb://3ec012f5b4d2/kb/decisions/x/abc.md",  // 4: the same local fact, qualified: dropped
		"kb://3ec012f5b4d2/kb/invariants/y/def.md", // 5: local, qualified
	}, local)
	want := []refSpec{
		{ord: 1, repo: "7b4887ce51d9", path: "kb/invariants/z/ghi.md"},
		{ord: 2, repo: "", path: "kb/decisions/x/abc.md"},
		{ord: 5, repo: "", path: "kb/invariants/y/def.md"},
	}
	if len(got) != len(want) {
		t.Fatalf("refSpecsOf = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("refSpecsOf = %+v, want %+v", got, want)
		}
	}
}
