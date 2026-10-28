package materialize

import "testing"

// Parents point to strictly older frames, so a cycle among ancestors can
// only form among frames created in the SAME commit. The fallback that breaks
// it must never lift a frame above a parent it does not share the cycle
// with: review N2's shape F→X, X→Y, Y↔Z, found in the order X, Y, Z.
func TestTopoSort_ACycleBelowNeverLiftsAChild(t *testing.T) {
	x := &node{key: key{"r", "x"}, parents: []key{{"r", "y"}}}
	y := &node{key: key{"r", "y"}, parents: []key{{"r", "z"}}}
	z := &node{key: key{"r", "z"}, parents: []key{{"r", "y"}}}
	got := topoSort([]*node{x, y, z})
	order := map[*node]int{}
	for i, n := range got {
		order[n] = i
	}
	if len(got) != 3 {
		t.Fatalf("every ancestor once, got %d", len(got))
	}
	if order[x] < order[y] {
		t.Fatalf("X cites Y: X must come after Y, got %v", []string{got[0].rel, got[1].rel, got[2].rel})
	}
	if got[0] != y {
		t.Fatalf("the cycle is broken at its earliest-found member, Y; got %s", got[0].rel)
	}
}

// With no cycle, it is Kahn's order with ties by discovery: T before both
// its children, P2 before P1 because P2 was found first.
func TestTopoSort_KahnTiesByDiscovery(t *testing.T) {
	tp := &node{key: key{"r", "t"}}
	p2 := &node{key: key{"r", "p2"}, parents: []key{{"r", "t"}}}
	p1 := &node{key: key{"r", "p1"}, parents: []key{{"r", "t"}}}
	got := topoSort([]*node{p2, p1, tp})
	if got[0] != tp || got[1] != p2 || got[2] != p1 {
		t.Fatalf("want t, p2, p1; got %s, %s, %s", got[0].rel, got[1].rel, got[2].rel)
	}
}

// Body headings move below the ### fact heading and stay REAL markdown:
// level n becomes min(n+3, 6). Nothing inside a ``` or ~~~ fence moves, and
// a fence closes only with the same character.
func TestShiftHeadings(t *testing.T) {
	in := "# One\n## Two\n### Three\n###### Six\n#\nnot # a heading\n#nospace\n" +
		"~~~\n# in tilde fence\n```\n# still in it\n~~~\n" +
		"```go\n## in backtick fence\n```\n## after"
	want := "#### One\n##### Two\n###### Three\n###### Six\n####\nnot # a heading\n#nospace\n" +
		"~~~\n# in tilde fence\n```\n# still in it\n~~~\n" +
		"```go\n## in backtick fence\n```\n##### after"
	if got := shiftHeadings(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
