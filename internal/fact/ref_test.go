package fact

import "testing"

// localID is "this repo" for the classification tests.
const localID = "3ec012f5b4d2"

const (
	testCommit40 = "4154e92c8ff333435fd00c442489e855e4c3331e"
	testBlob40   = "36b1d45187d6a2c6ad18d591142227ad2a02a66e"
)

func TestClassifyRef(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Ref
	}{
		{
			name: "bare local fact path",
			raw:  "kb/decisions/lens/qualified-path-repo-identity/10a3bcc0.md",
			want: Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/decisions/lens/qualified-path-repo-identity/10a3bcc0.md"},
		},
		{
			name: "kb:// pointing at THIS repo is local, not external",
			raw:  "kb://3ec012f5b4d2/kb/invariants/store/x/abc123.md",
			want: Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/invariants/store/x/abc123.md"},
		},
		{
			name: "kb:// pointing at another repo is foreign",
			raw:  "kb://7b4887ce51d9/kb/invariants/store/x/abc123.md",
			want: Ref{Kind: RefForeignFact, RepoID: "7b4887ce51d9", Path: "kb/invariants/store/x/abc123.md"},
		},
		{
			name: "src new form: id, commit, blob",
			raw:  "src://7b4887ce51d9/internal/store/git/storer.go@" + testCommit40 + ":" + testBlob40,
			want: Ref{
				Kind: RefSourceCode, RepoID: "7b4887ce51d9", Path: "internal/store/git/storer.go",
				Commit: testCommit40, Blob: testBlob40,
			},
		},
		{
			name: "src new form with line range",
			raw:  "src://7b4887ce51d9/internal/repos/instance.go@" + testCommit40 + ":" + testBlob40 + "#L241-L259",
			want: Ref{
				Kind: RefSourceCode, RepoID: "7b4887ce51d9", Path: "internal/repos/instance.go",
				Commit: testCommit40, Blob: testBlob40, Lines: "241-259",
			},
		},
		{
			name: "src legacy: repo NAME and a bare commit",
			raw:  "src://knomit/internal/store/repo.go@ca1c272",
			want: Ref{Kind: RefSourceCode, RepoID: "knomit", Path: "internal/store/repo.go", Commit: "ca1c272", Legacy: true},
		},
		{
			name: "src legacy: no commit at all",
			raw:  "src://knomit/internal/repos/auth.go",
			want: Ref{Kind: RefSourceCode, RepoID: "knomit", Path: "internal/repos/auth.go", Legacy: true},
		},
		{
			name: "https is external",
			raw:  "https://github.com/knomit/knomit/discussions/8",
			want: Ref{Kind: RefExternalURL},
		},
		{
			name: "file:// is external",
			raw:  "file:///Users/pba/notes.md",
			want: Ref{Kind: RefExternalURL},
		},
		{
			name: "a path that does not exist is STILL a local fact ref — kind is syntax, not existence",
			raw:  "kb/nope.md",
			want: Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/nope.md"},
		},
		{
			name: "malformed kb:// — id wrong length",
			raw:  "kb://abc/kb/x.md",
			want: Ref{Kind: RefMalformed, Err: `malformed kb:// path "kb://abc/kb/x.md" — want kb://<12-hex-repo-id>/<path>`},
		},
		{
			name: "malformed kb:// — no path after id",
			raw:  "kb://3ec012f5b4d2/",
			want: Ref{Kind: RefMalformed, Err: `malformed kb:// path "kb://3ec012f5b4d2/" — want kb://<12-hex-repo-id>/<path>`},
		},
		{
			name: "malformed src:// — empty path",
			raw:  "src://7b4887ce51d9/",
			want: Ref{Kind: RefMalformed, Err: `malformed src:// ref "src://7b4887ce51d9/" — want src://<repo>/<path>[@<commit>[:<blob>]]`},
		},
		{
			name: "empty string is malformed",
			raw:  "",
			want: Ref{Kind: RefMalformed, Err: "empty ref"},
		},

		// Casing. Fact paths are lowercase-canonical in storage and every
		// lookup that consumes Path is case-sensitive; source paths are real
		// filenames in a case-sensitive tree and must NOT be touched.
		{
			name: "bare fact path is lowercased",
			raw:  "kb/Decisions/X/Abc.md",
			want: Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/decisions/x/abc.md"},
		},
		{
			name: "kb:// with uppercase id and path is fully lowercased",
			raw:  "kb://3EC012F5B4D2/kb/X.md",
			want: Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/x.md"},
		},
		{
			name: "src path casing is PRESERVED — FactBody.tsx is a real filename",
			raw:  "src://7b4887ce51d9/internal/web/FactBody.tsx@" + testCommit40 + ":" + testBlob40,
			want: Ref{
				Kind: RefSourceCode, RepoID: "7b4887ce51d9", Path: "internal/web/FactBody.tsx",
				Commit: testCommit40, Blob: testBlob40,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRef(tc.raw, localID)
			tc.want.Raw = tc.raw // Raw is always byte-identical to the input.
			if got != tc.want {
				t.Errorf("ClassifyRef(%q)\n got  %+v\n want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// A bare path classifies local even when the caller does not know its own id —
// the id is unknown, but the KIND is not in doubt.
func TestClassifyRef_EmptyLocalID(t *testing.T) {
	got := ClassifyRef("kb/x/y.md", "")
	if got.Kind != RefLocalFact {
		t.Fatalf("kind = %v, want %v", got.Kind, RefLocalFact)
	}
	if got.RepoID != "" {
		t.Errorf("RepoID = %q, want empty", got.RepoID)
	}

	// With no local id, every kb:// ref is foreign. That is the safe direction:
	// consumers under-report local refs rather than inventing them.
	if k := ClassifyRef("kb://3ec012f5b4d2/kb/x.md", "").Kind; k != RefForeignFact {
		t.Errorf("kb:// with empty localRepoID: kind = %v, want %v", k, RefForeignFact)
	}
}

func TestParseKBPath(t *testing.T) {
	id, rel, qualified, err := ParseKBPath("kb://3ec012f5b4d2/kb/x/y.md")
	if err != nil || !qualified || id != "3ec012f5b4d2" || rel != "kb/x/y.md" {
		t.Fatalf("qualified: got (%q,%q,%v,%v)", id, rel, qualified, err)
	}

	id, rel, qualified, err = ParseKBPath("kb/x/y.md")
	if err != nil || qualified || id != "" || rel != "kb/x/y.md" {
		t.Fatalf("bare: got (%q,%q,%v,%v)", id, rel, qualified, err)
	}

	// A pasted uppercase hash parses; canonical form is lowercase.
	id, _, _, err = ParseKBPath("kb://3EC012F5B4D2/kb/x.md")
	if err != nil || id != "3ec012f5b4d2" {
		t.Fatalf("uppercase id: got (%q,%v)", id, err)
	}

	if _, _, _, err = ParseKBPath("kb://abc/x.md"); err == nil {
		t.Error("short id: want error")
	}
	if _, _, _, err = ParseKBPath("kb://3ec012f5b4d2/"); err == nil {
		t.Error("empty remainder: want error")
	}
}

func TestParseLineRange(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"L241", "241", true},
		{"L241-L259", "241-259", true},
		{"L241-259", "241-259", true},
		{"241", "", false},    // no L prefix
		{"Labc", "", false},   // not digits
		{"L241-x", "", false}, // bad end
		{"", "", false},
	} {
		got, ok := parseLineRange(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseLineRange(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A '#' that is not a line range stays part of the path — '#' is legal in a
// filename, so only a well-formed range may be stripped.
func TestClassifySrc_HashInPathIsNotALineRange(t *testing.T) {
	raw := "src://7b4887ce51d9/docs/c#-notes.md@" + testCommit40 + ":" + testBlob40
	got := ClassifyRef(raw, localID)
	if got.Kind != RefSourceCode {
		t.Fatalf("kind = %v, want %v", got.Kind, RefSourceCode)
	}
	if got.Path != "docs/c#-notes.md" {
		t.Errorf("Path = %q, want docs/c#-notes.md", got.Path)
	}
	if got.Lines != "" {
		t.Errorf("Lines = %q, want empty", got.Lines)
	}
}

func TestID12AndQualify(t *testing.T) {
	if got := ID12("3ec012f5b4d2ffffffff"); got != "3ec012f5b4d2" {
		t.Errorf("ID12 = %q", got)
	}
	if got := ID12("abc"); got != "abc" {
		t.Errorf("ID12 short = %q", got)
	}
	if got := QualifyKBPath("3ec012f5b4d2", "kb/x.md"); got != "kb://3ec012f5b4d2/kb/x.md" {
		t.Errorf("QualifyKBPath = %q", got)
	}
}

// Display is the label side of a ref: what a human reads, never what a tool
// resolves. The stored Raw stays full-width (see gitHashLen) precisely so this
// abbreviation can be lossy without costing anyone the citation.
func TestRefDisplay(t *testing.T) {
	const (
		commit = "8cba88ff2e1c0556c90b1c9b21574772303b28cf"
		blob   = "c451fd992c42a2f30f0db62108259c0647b773dc"
	)
	cases := []struct {
		name     string
		raw      string
		repoName string
		want     string
	}{{
		name:     "src, id resolved to a name, both hashes abbreviated",
		raw:      "src://7b4887ce51d9/internal/refs/refs.go@" + commit + ":" + blob,
		repoName: "knomit",
		want:     "src://knomit/internal/refs/refs.go@8cba88ff…:c451fd99…",
	}, {
		name: "src, id unresolved: the id stays rather than being dropped",
		raw:  "src://7b4887ce51d9/internal/refs/refs.go@" + commit + ":" + blob,
		want: "src://7b4887ce51d9/internal/refs/refs.go@8cba88ff…:c451fd99…",
	}, {
		name:     "src with a line range keeps it",
		raw:      "src://7b4887ce51d9/x.go@" + commit + ":" + blob + "#L241-L259",
		repoName: "knomit",
		want:     "src://knomit/x.go@8cba88ff…:c451fd99…#L241-259",
	}, {
		name: "src legacy: a commit already shorter than the cut gains no ellipsis",
		raw:  "src://knomit/internal/legacy.go@ca1c272",
		want: "src://knomit/internal/legacy.go@ca1c272",
	}, {
		name: "src legacy with no version at all",
		raw:  "src://knomit/internal/legacy.go",
		want: "src://knomit/internal/legacy.go",
	}, {
		name:     "foreign kb ref gets the name overlay, and has no hashes to cut",
		raw:      "kb://7b4887ce51d9/kb/z.md",
		repoName: "knomit",
		want:     "kb://knomit/kb/z.md",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// localRepoID "" so a kb:// ref classifies foreign, which is the
			// case with something to display.
			if got := ClassifyRef(tc.raw, "").Display(tc.repoName); got != tc.want {
				t.Errorf("Display(%q) = %q, want %q", tc.repoName, got, tc.want)
			}
		})
	}
}

// The kinds that must NOT get a display form. A local fact is rendered by its
// repo-relative Path, so a competing shortening could only diverge from it; a
// URL truncated mid-string is no longer followable.
func TestRefDisplay_EmptyForKindsWithNothingToShorten(t *testing.T) {
	const local = "3ec012f5b4d2"
	for _, raw := range []string{
		"kb/topic/abc123.md",
		"kb://" + local + "/kb/topic/abc123.md",
		"https://example.com/a/very/long/path/that/must/not/be/cut",
		"file:///tmp/notes.txt",
		"", // malformed
	} {
		if got := ClassifyRef(raw, local).Display("knomit"); got != "" {
			t.Errorf("Display(%q) = %q, want empty", raw, got)
		}
	}
}
