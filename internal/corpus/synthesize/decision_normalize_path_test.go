package synthesize

import (
	"regexp"
	"strings"
	"testing"
)

// normalizeFactPath must produce a git-style, forward-slashed fact path on
// every OS, because that is what a fact path IS — the name of a blob in a
// tree, not a file on this machine.
//
// This is the regression test for a bug that produced NOTHING and said
// nothing. The function used filepath.Dir, which answers in the host
// separator, so on Windows "kb/synthesis/x.md" came back "kb\synthesis" and
// the result was the mongrel "kb\synthesis/<uuid>.md". validateOutputPath then
// tested it for the "kb/" prefix, did not find it, and rejected the write —
// via the progress channel as a warn, not as an error. Every merge, distill,
// propose and emergent-fact write on Windows was silently dropped.
//
// The assertion is on the whole shape rather than on "no backslashes",
// because the failure was a path that was half right.
func TestNormalizeFactPathIsAlwaysSlashSeparated(t *testing.T) {
	for _, tc := range []struct{ in, wantDir string }{
		{"kb/synthesis/x.md", "kb/synthesis"},
		{"kb/technology/security/note.md", "kb/technology/security"},
		{"kb/top.md", "kb"},
	} {
		got := normalizeFactPath(tc.in)

		want := regexp.MustCompile(`^` + regexp.QuoteMeta(tc.wantDir) + `/[0-9a-f]{8}\.md$`)
		if !want.MatchString(got) {
			t.Errorf("normalizeFactPath(%q) = %q, want %v", tc.in, got, want)
		}
		if strings.ContainsRune(got, '\\') {
			t.Errorf("normalizeFactPath(%q) = %q — a fact path is git-style and never contains a backslash", tc.in, got)
		}
	}
}

// The output has to survive the gate that rejected it. Pairing the two here
// pins the actual invariant: what normalizeFactPath emits, validateOutputPath
// accepts. Either function changing alone re-breaks the write path.
func TestNormalizedPathPassesTheOutputGate(t *testing.T) {
	const root = "kb"
	for _, in := range []string{
		"kb/synthesis/x.md",
		"kb/technology/security/vulnerabilities/note.md",
	} {
		if err := validateOutputPath(normalizeFactPath(in), root); err != nil {
			t.Errorf("validateOutputPath(normalizeFactPath(%q)) = %v; the merge would be dropped with only a warn", in, err)
		}
	}
}
