package fact

import (
	"strings"
	"testing"
)

// TestClassifyRef_SystemFile pins the two system-file kinds: a ref whose
// repo-relative path is under .knomit/ names a FILE, kept verbatim (case
// included), local iff bare or kb://<own-id>/…; a malformed one ("..", an
// empty segment, a trailing /, the bare root) is RefMalformed; and every OTHER
// dot path stays a fact-kind ref, closed as before.
// Sabotage: drop the isUnderPrivateRoot branch in ClassifyRef → the bare and
// own-id cases classify as local_fact, lowercased → red.
func TestClassifyRef_SystemFile(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Ref
	}{
		{"bare", ".knomit/ontology.yaml",
			Ref{Kind: RefLocalSystemFile, RepoID: localID, Path: ".knomit/ontology.yaml"}},
		{"bare, case kept, nested dot dir", ".knomit/templates/mission/.knomit/skills/post-task/SKILL.md",
			Ref{Kind: RefLocalSystemFile, RepoID: localID, Path: ".knomit/templates/mission/.knomit/skills/post-task/SKILL.md"}},
		{"kb:// own id is local", "kb://" + localID + "/.knomit/triggers/Claims.js",
			Ref{Kind: RefLocalSystemFile, RepoID: localID, Path: ".knomit/triggers/Claims.js"}},
		{"kb:// other id is foreign", "kb://7b4887ce51d9/.knomit/recipes/x.js",
			Ref{Kind: RefForeignSystemFile, RepoID: "7b4887ce51d9", Path: ".knomit/recipes/x.js"}},
		{".github stays a fact ref", ".github/x",
			Ref{Kind: RefLocalFact, RepoID: localID, Path: ".github/x"}},
		{"kb/.drafts stays a fact ref", "kb/.drafts/x.md",
			Ref{Kind: RefLocalFact, RepoID: localID, Path: "kb/.drafts/x.md"}},
		{"uppercase root is not the system root", ".KNOMIT/x.yaml",
			Ref{Kind: RefLocalFact, RepoID: localID, Path: ".knomit/x.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRef(tt.raw, localID)
			tt.want.Raw = tt.raw
			if got != tt.want {
				t.Fatalf("ClassifyRef(%q)\n got  %+v\n want %+v", tt.raw, got, tt.want)
			}
		})
	}

	for _, raw := range []string{
		".knomit/../kb/x.md",
		".knomit/a/../b",
		".knomit/a//b",
		".knomit/a/",
		".knomit/",
		".knomit",
		".knomit/./x",
		"kb://" + localID + "/.knomit/a/../b",
	} {
		t.Run("malformed "+raw, func(t *testing.T) {
			got := ClassifyRef(raw, localID)
			if got.Kind != RefMalformed || got.Err == "" {
				t.Fatalf("ClassifyRef(%q) = %+v, want malformed with a reason", raw, got)
			}
		})
	}
}

// TestValidateRefs_SystemFileForm: a well-formed system-file ref passes the
// write-side shape rule, a malformed one is refused, and the refusal lists
// the system-file forms among the accepted ones.
// Sabotage: delete the two system-file lines from ValidateRefs' message → red.
func TestValidateRefs_SystemFileForm(t *testing.T) {
	if err := ValidateRefs([]string{".knomit/ontology.yaml", "kb://7b4887ce51d9/.knomit/skills/x/SKILL.md"}); err != nil {
		t.Fatalf("well-formed system-file refs refused: %v", err)
	}
	err := ValidateRefs([]string{".knomit/a/../b"})
	if err == nil {
		t.Fatal("a .. segment must be refused")
	}
	for _, want := range []string{`".knomit/a/../b"`, ".knomit/<path>", "kb://<12-hex-repo-id>/.knomit/<path>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q\n--- got ---\n%v", want, err)
		}
	}
}

// TestRefIdentity_SystemFile: bare and own-id forms of one file compare
// equal; case is significant; a fact path never collides with a file path.
func TestRefIdentity_SystemFile(t *testing.T) {
	a := RefIdentity(".knomit/skills/x/SKILL.md", localID)
	b := RefIdentity("kb://"+localID+"/.knomit/skills/x/SKILL.md", localID)
	if a != b {
		t.Fatalf("bare %q and canonical %q must be one identity", a, b)
	}
	if a == RefIdentity(".knomit/skills/x/skill.md", localID) {
		t.Fatal("case must be significant for a system file")
	}
	if RefIdentity("kb://7b4887ce51d9/.knomit/skills/x/SKILL.md", localID) == a {
		t.Fatal("a foreign file must not equal a local one")
	}
}

func TestIsSystemFilePath(t *testing.T) {
	for p, want := range map[string]bool{
		".knomit/ontology.yaml":                  true,
		".knomit/templates/mission/.knomit/x.js": true,
		".knomit/skills/program-knomit/SKILL.md": true,
		".knomit":                                false,
		".knomit/":                               false,
		".knomit//x":                             false,
		".knomit/x/":                             false,
		".knomit/../x":                           false,
		".knomit/./x":                            false,
		".KNOMIT/x":                              false,
		".github/x":                              false,
		"kb/.knomit/x":                           false,
		"kb://3ec012f5b4d2/.knomit/x":            false, // bare paths only
		"artifacts/runs/x.md":                    false,
	} {
		if got := IsSystemFilePath(p); got != want {
			t.Errorf("IsSystemFilePath(%q) = %v, want %v", p, got, want)
		}
	}
}
