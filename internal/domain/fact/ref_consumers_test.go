package fact_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"knomit/internal/testsupport/modroot"
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
		filepath.Join("internal", "domain", "fact", "ref.go"):                "the authority itself",
		filepath.Join("internal", "domain", "fact", "ref_test.go"):           "tests the authority",
		filepath.Join("internal", "domain", "fact", "ref_consumers_test.go"): "this guard",

		// Not ref classification. These decide whether a PATH names a fact FILE, or
		// validate a git remote URL — filename and URL questions, not reference
		// questions. Each is listed with its reason so a future addition has to
		// justify itself rather than silently widening the exemption.
		filepath.Join("internal", "domain", "fact", "corpus_test.go"):                  "walks a directory selecting .md files",
		filepath.Join("internal", "domain", "fact", "paths.go"):                        "fact file naming",
		filepath.Join("internal", "corpus", "synthesize", "pipeline.go"):               "skips non-fact files when scanning a tree",
		filepath.Join("internal", "domain", "okf", "source", "history.go"):             "selects fact files out of a git tree",
		filepath.Join("internal", "engine", "store", "fleetrec", "read.go"):            "selects member record files out of a fleet repository's git tree",
		filepath.Join("internal", "engine", "store", "verify", "range.go"):             "selects the member record files a judged commit writes (record-owner rule)",
		filepath.Join("internal", "engine", "store", "fleetrec", "activity.go"):        "selects the member record files a commit of the fleet's main changes (GET /fleet/activity)",
		filepath.Join("internal", "engine", "store", "facts", "templates.go"):          "selects template fact files out of a source repository's git tree (F24)",
		filepath.Join("internal", "domain", "okf", "source", "facts.go"):               "selects fact files out of a git tree",
		filepath.Join("internal", "domain", "okf", "validate.go"):                      "validates a fact FILE name",
		filepath.Join("internal", "domain", "okf", "concept.go"):                       "http(s)-only followability, after ClassifyRef has decided the kind",
		filepath.Join("internal", "engine", "store", "internal", "vcs", "branch.go"):   "selects fact files from a git tree",
		filepath.Join("internal", "engine", "store", "internal", "vcs", "factpath.go"): "index membership by location under the ontology root",
		filepath.Join("internal", "engine", "store", "facts", "fact_read.go"):          "fact file naming",
		filepath.Join("internal", "engine", "store", "git", "commitlog.go"):            "selects fact files when indexing a commit",
		filepath.Join("internal", "corpus", "repos", "origin_method.go"):               "checks a remote URL's scheme against the auth method (MethodMismatch, shared with web httpx.ValidateURLAuth), not a ref",
		filepath.Join("internal", "edge", "mcp", "lens_endtoend_test.go"):              "asserts the kb:// WIRE PATH form of a result row",
		filepath.Join("internal", "edge", "mcp", "query_federation_test.go"):           "asserts the kb:// WIRE PATH form of a result row",
	}

	root := modroot.Dir(t)
	var offenders []string
	walked := 0
	seenAllowed := map[string]bool{}

	walk := func(sub string) error {
		return filepath.Walk(filepath.Join(root, sub), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			walked++
			if _, ok := allowed[rel]; ok {
				seenAllowed[rel] = true
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

	// Non-vacuity: the scan must have reached the tree, and every allowlist key
	// must name a file it walked (a stale key silently exempts nothing today
	// and a future file of that name everything).
	if walked < 100 {
		t.Fatalf("walked only %d .go files under internal/ and test/ — the guard is not watching the tree", walked)
	}
	for k := range allowed {
		if !seenAllowed[k] {
			t.Errorf("allowed key %s was never walked — stale entry or moved file", k)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("ref classification appears to be re-implemented outside internal/domain/fact:\n  %s\n\n"+
			"Use fact.ClassifyRef. If a file genuinely needs a raw scheme or suffix check for "+
			"something OTHER than classifying a reference, add it to the allowed map with a "+
			"one-line reason.",
			strings.Join(offenders, "\n  "))
	}
}
