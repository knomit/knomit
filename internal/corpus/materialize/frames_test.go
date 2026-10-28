package materialize

import "testing"

// Kahn's order with ties by discovery: T before both
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
