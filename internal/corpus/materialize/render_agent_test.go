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

// The export name is <slug, at most 48 characters, cut at a word>-<id8>.md.
//
// Sabotage: cut the slug at 48 bytes without looking for a "-", and the
// "defaults" case keeps half a word ("...-by-default"); use the raw file id
// (shortID) for the id part, and the quote and multibyte cases break.
func TestExportFileName(t *testing.T) {
	for _, c := range []struct{ title, root, want string }{
		{"materialize rulings 2026 10 28 pinned by defaults unpin", "kb/a/0123abcd.md",
			"materialize-rulings-2026-10-28-pinned-by-0123abcd.md"},
		{"materialize rulings (2026-10-28): pinned by default, unpin silently drops gone targets",
			"kb/decisions/materialize/rulings/16ccefe6.md", "materialize-rulings-2026-10-28-pinned-by-default-16ccefe6.md"},
		{"Short Title!", "kb/a/0123abcd.md", "short-title-0123abcd.md"},
		{"  --Ünïcode tïtle-- ", "kb://3ec012f5b4d2/kb/a/deadbeef.md", "n-code-t-tle-deadbeef.md"},
		{"日本語", "kb/a/0123abcd.md", "0123abcd.md"},
		{"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123", "kb/a/0123abcd.md",
			"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuv-0123abcd.md"},
		{"exactly forty eight characters long title abcdef zz", "kb/a/0123abcd.md",
			"exactly-forty-eight-characters-long-title-abcdef-0123abcd.md"},
		// The id goes through the same rule: a hand-authored file name with
		// a quote cannot break the quoted filename, and the 8-character cut
		// never splits a multibyte character (it is made on the ASCII slug).
		{"Quoted", `kb/a/ab"cd.md`, "quoted-ab-cd.md"},
		{"Multi", "kb/a/abc日本defgh.md", "multi-abc-defg.md"},
		{"Multi", "kb/a/日本語.md", "multi.md"},
		{"日本", "kb/a/日本語.md", "materialize.md"},
	} {
		if got := ExportFileName(&Document{RootTitle: c.title, Root: c.root}); got != c.want {
			t.Errorf("%q: got %q, want %q", c.title, got, c.want)
		}
	}
}
