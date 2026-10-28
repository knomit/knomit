package materialize

import "testing"

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
