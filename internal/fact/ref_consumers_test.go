package fact_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Ref classification must have exactly ONE implementation. Eight independent
// copies of the rule is what this work removed, and they disagreed: a
// cross-repo kb://<other>/z.md ref rendered as BROKEN in the UI, a markdown
// source citation counted as a local lineage edge, and replay silently DELETED
// every src:// ref it saw. A ninth copy must fail here rather than in
// production.
//
// Modelled on TestFactSchema_DescriptionsAreComplete: a structural guard, not
// a behavioural one.
//
// test/ is walked as well as internal/ — one of the copies lived in
// test/testenv/follow_ref.go, and a guard watching only internal/ would not
// have caught it.
func TestRefClassification_HasNoSecondImplementation(t *testing.T) {
	banned := []*regexp.Regexp{
		regexp.MustCompile(`HasPrefix\([^,)]+,\s*"https?://"\)`),
		regexp.MustCompile(`HasPrefix\([^,)]+,\s*"src://"\)`),
		regexp.MustCompile(`HasPrefix\([^,)]+,\s*federate\.KBScheme\)`),
		regexp.MustCompile(`HasSuffix\([^,)]+,\s*"\.md"\)`),
	}

	// Files allowed to carry these patterns, each for a stated reason.
	allowed := map[string]string{
		filepath.Join("internal", "fact", "ref.go"):                "the authority itself",
		filepath.Join("internal", "fact", "ref_test.go"):           "tests the authority",
		filepath.Join("internal", "fact", "ref_consumers_test.go"): "this guard",

		// Not ref classification. These decide whether a PATH names a fact FILE, or
		// validate a git remote URL — filename and URL questions, not reference
		// questions. Each is listed with its reason so a future addition has to
		// justify itself rather than silently widening the exemption.
		filepath.Join("internal", "fact", "corpus_test.go"):          "walks a directory selecting .md files",
		filepath.Join("internal", "fact", "paths.go"):                "fact file naming",
		filepath.Join("internal", "synthesize", "pipeline.go"):       "skips non-fact files when scanning a tree",
		filepath.Join("internal", "okf", "source", "history.go"):     "selects fact files out of a git tree",
		filepath.Join("internal", "okf", "source", "facts.go"):       "selects fact files out of a git tree",
		filepath.Join("internal", "okf", "validate.go"):              "validates a fact FILE name",
		filepath.Join("internal", "okf", "concept.go"):               "http(s)-only followability, after ClassifyRef has decided the kind",
		filepath.Join("internal", "store", "branch.go"):              "selects fact files from a git tree",
		filepath.Join("internal", "store", "factpath.go"):            "index membership by location under the ontology root",
		filepath.Join("internal", "store", "fact_read.go"):           "fact file naming",
		filepath.Join("internal", "store", "git", "commitlog.go"):    "selects fact files when indexing a commit",
		filepath.Join("internal", "web", "helpers.go"):               "validates a git REMOTE url, not a ref",
		filepath.Join("internal", "mcp", "lens_endtoend_test.go"):    "asserts the kb:// WIRE PATH form of a result row",
		filepath.Join("internal", "mcp", "query_federation_test.go"): "asserts the kb:// WIRE PATH form of a result row",
	}

	root := "../.."
	var offenders []string

	walk := func(sub string) error {
		return filepath.Walk(filepath.Join(root, sub), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			if _, ok := allowed[rel]; ok {
				return nil
			}
			src, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			for _, re := range banned {
				if re.Match(src) {
					offenders = append(offenders, rel+"  matches  "+re.String())
				}
			}
			return nil
		})
	}

	for _, sub := range []string{"internal", "test"} {
		if err := walk(sub); err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("ref classification appears to be re-implemented outside internal/fact:\n  %s\n\n"+
			"Use fact.ClassifyRef. If a file genuinely needs a raw scheme or suffix check for "+
			"something OTHER than classifying a reference, add it to the allowed map with a "+
			"one-line reason.",
			strings.Join(offenders, "\n  "))
	}
}
