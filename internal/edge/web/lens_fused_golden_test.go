package web

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// Characterization goldens for the fused lens reads, /lenses/{lens}/search and
// /lenses/{lens}/facts. They pin the WIRE BYTES (status line plus body) of every
// case the fused search covers, so moving the fan-out, fusion and dedupe into
// one shared function cannot change a byte unnoticed. The MCP twin is
// internal/edge/mcp/query_fused_golden_test.go.
//
// Only three things are rewritten before the comparison, all of them values a
// fresh store mints differently on every run: each mount's 12-hex repo id (to
// <id:name>), each committed_at stamp (to its rank among the distinct stamps of
// the response, oldest = 1), and a full commit hash (to <sha>). Everything else
// — field order, scores, paths, order of rows, problem documents — is compared
// byte for byte.
//
// Regenerate with: go test ./internal/edge/web/ -run TestLensFused -update-lens-fused

var updateLensFused = flag.Bool("update-lens-fused", false, "rewrite testdata/lens_fused/*.golden")

var (
	fusedStampRe = regexp.MustCompile(`"committed_at":(\d+)`)
	fusedSHARe   = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
)

// normalizeFused rewrites the per-run values of one response body (see above).
func normalizeFused(body string, ids map[string]string) string {
	for id12, name := range ids {
		body = strings.ReplaceAll(body, id12, "<id:"+name+">")
	}
	body = fusedSHARe.ReplaceAllString(body, "<sha>")
	var stamps []int64
	seen := map[int64]bool{}
	for _, m := range fusedStampRe.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if !seen[n] {
			seen[n] = true
			stamps = append(stamps, n)
		}
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i] < stamps[j] })
	rank := map[int64]int{}
	for i, s := range stamps {
		rank[s] = i + 1
	}
	return fusedStampRe.ReplaceAllStringFunc(body, func(s string) string {
		n, _ := strconv.ParseInt(fusedStampRe.FindStringSubmatch(s)[1], 10, 64)
		return `"committed_at":"<t` + strconv.Itoa(rank[n]) + `>"`
	})
}

// fusedIDs maps each named mount's id12 to its name, for normalizeFused.
func fusedIDs(m *repos.Manager, names ...string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		out[federate.ID12(m.Get(n).ID())] = n
	}
	return out
}

// checkFusedGolden GETs every url, renders "== url / status / body" blocks and
// compares the whole transcript with testdata/lens_fused/<name>.golden.
func checkFusedGolden(t *testing.T, env searchLensEnv, ids map[string]string, name string, urls ...string) {
	t.Helper()
	var b strings.Builder
	for _, u := range urls {
		rec := env.get(t, u)
		fmt.Fprintf(&b, "== GET %s\nstatus: %d\n%s\n", normalizeFused(u, ids), rec.Code, normalizeFused(strings.TrimRight(rec.Body.String(), "\n"), ids))
	}
	got := b.String()
	path := filepath.Join("testdata", "lens_fused", name+".golden")
	if *updateLensFused {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-lens-fused): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s: wire bytes differ from %s\n--- want\n%s\n--- got\n%s", name, path, want, got)
	}
}

// seedFusedCorpus writes the three-mount corpus every golden case reads.
// Mounts sort by name, so alpha is mount 0, mid (the WRITE repo) mount 1 and
// zulu mount 2: the write repo is deliberately NOT first in binding order.
//
//   - kb/shared/1.md lives on alpha AND mid; alpha's copy scores higher, mid's
//     copy must win (write-first dedupe), even though alpha's ranks first.
//   - alpha carries `config-drift`, zulu carries `drift-config`: one cluster
//     mechanically, two elected spellings, so only the lens's widening makes a
//     `config-drift` filter reach zulu.
//   - equal per-mount ranks across mounts exercise the RRF mount tie-break.
//   - each mount is one commit, in its own second, so the recency order is total.
func seedFusedCorpus(t *testing.T, m *repos.Manager) {
	t.Helper()
	policy := func(f *knomitfact.Fact) { f.Kind = knomitfact.Pragmatic; f.Type = knomitfact.Policy }
	writeFactsInOneCommit(t, m.Get("alpha"),
		factSpec{path: "kb/shared/1.md", title: "alpha shared copy", cos: 0.99},
		factSpec{path: "kb/a/1.md", title: "alpha one", cos: 0.8, mod: withMotifs("config-drift")},
		factSpec{path: "kb/a/2.md", title: "alpha two", cos: 0.6},
	)
	awaitNextSecond()
	writeFactsInOneCommit(t, m.Get("mid"),
		factSpec{path: "kb/shared/1.md", title: "mid shared copy", cos: 0.5},
		factSpec{path: "kb/m/1.md", title: "mid one", cos: 0.9, mod: func(f *knomitfact.Fact) { f.Domain = []string{"store"} }},
	)
	awaitNextSecond()
	writeFactsInOneCommit(t, m.Get("zulu"),
		factSpec{path: "kb/z/1.md", title: "zulu one", cos: 0.95, mod: withMotifs("drift-config")},
		factSpec{path: "kb/z/2.md", title: "zulu two", cos: 0.7, mod: policy},
	)
}

func TestLensFused_SearchAndFactsGolden(t *testing.T) {
	env := searchLens(t, "mid", "alpha", "zulu")
	seedFusedCorpus(t, env.m)
	ids := fusedIDs(env.m, "alpha", "mid", "zulu")

	checkFusedGolden(t, env, ids, "corpus",
		// relevance: semantic text, RRF over three mounts, write-first dedupe
		"/lenses/eng/search?q=x",
		"/lenses/eng/search?q=x&limit=2",
		// filter-only (no text)
		"/lenses/eng/search?type=observation",
		// repo= narrowing, and its 422
		"/lenses/eng/search?q=x&repo=alpha&repo=zulu",
		"/lenses/eng/search?q=x&repo=nope",
		// motif widening (zulu answers only through the union)
		"/lenses/eng/search?q=x&motifs=config-drift",
		// a path filter naming no mount: 400 from target selection
		"/lenses/eng/search?q=x&path=kb://000000000000/kb/",
		// a kb://-qualified path restricts to one mount
		"/lenses/eng/search?q=x&path=kb://"+federate.ID12(env.m.Get("zulu").ID())+"/kb/",
		// a min_similarity that removes every candidate: the notice
		"/lenses/eng/search?q=x&min_similarity=0.999",
		// recency listing: committed_at merge, then RRF when text is present
		"/lenses/eng/facts",
		"/lenses/eng/facts?q=x",
		"/lenses/eng/facts?limit=2&offset=1",
		"/lenses/eng/facts?repo=zulu",
		"/lenses/eng/facts?repo=nope",
		"/lenses/eng/facts?motifs=config-drift",
		"/lenses/eng/facts?type=policy",
	)
}

func TestLensFused_EmptyLensGolden(t *testing.T) {
	env := searchLens(t, "mid", "alpha")
	ids := fusedIDs(env.m, "alpha", "mid")
	checkFusedGolden(t, env, ids, "empty",
		"/lenses/eng/search?q=x",
		"/lenses/eng/search?type=observation",
		"/lenses/eng/facts",
		"/lenses/eng/facts?q=x",
	)
}

func TestLensFused_OneMemberLensGolden(t *testing.T) {
	env := searchLens(t, "solo")
	writeFactsInOneCommit(t, env.m.Get("solo"),
		factSpec{path: "kb/s/1.md", title: "solo one", cos: 0.7, mod: withMotifs("config-drift")},
		factSpec{path: "kb/s/2.md", title: "solo two", cos: 0.9, mod: withMotifs("drift-config")},
	)
	ids := fusedIDs(env.m, "solo")
	checkFusedGolden(t, env, ids, "one_member",
		"/lenses/eng/search?q=x",
		"/lenses/eng/search?q=x&motifs=config-drift",
		"/lenses/eng/facts",
		"/lenses/eng/facts?q=x",
	)
}

// failSlowMount makes this mount's named read wait d (the Overrides probe, a
// pure delay whose error the store ignores) and then fail with err (the read
// fault).
func failSlowMount(t *testing.T, m *repos.Manager, name string, read index.ReadName, d time.Duration, err error) {
	t.Helper()
	withMountStore(t, m, name, func(svc *store.Service) {
		installMountOverrides(t, svc, "failSlowMount", index.Overrides{Reads: func(got string) error {
			if got == string(read) {
				time.Sleep(d)
			}
			return nil
		}})
		t.Cleanup(svc.Search().SetReadFaultForTest(func(got index.ReadName) error {
			if got == read {
				return err
			}
			return nil
		}))
	})
}

// Two mounts fail. The LOWER-indexed one (mid, mount 1) finishes LAST, so a
// fan-out that reported the first failure to arrive would name zulu; the
// contract (b9c7ab96) is that the lowest index wins.
func TestLensFused_MemberFailureGolden(t *testing.T) {
	env := searchLens(t, "mid", "alpha", "zulu")
	seedFusedCorpus(t, env.m)
	ids := fusedIDs(env.m, "alpha", "mid", "zulu")
	failSlowMount(t, env.m, "mid", index.ReadSearch, 150*time.Millisecond, errors.New("mid search down"))
	failSlowMount(t, env.m, "zulu", index.ReadSearch, 0, errors.New("zulu search down"))
	checkFusedGolden(t, env, ids, "failure_search",
		"/lenses/eng/search?q=x",
		// repo= narrowing that excludes mid: zulu's failure is now the lowest
		"/lenses/eng/search?q=x&repo=alpha&repo=zulu",
	)
}

func TestLensFused_MemberFailureFactsGolden(t *testing.T) {
	env := searchLens(t, "mid", "alpha", "zulu")
	seedFusedCorpus(t, env.m)
	ids := fusedIDs(env.m, "alpha", "mid", "zulu")
	failSlowMount(t, env.m, "mid", index.ReadRecentFacts, 150*time.Millisecond, errors.New("mid recent down"))
	failSlowMount(t, env.m, "zulu", index.ReadRecentFacts, 0, errors.New("zulu recent down"))
	checkFusedGolden(t, env, ids, "failure_facts",
		"/lenses/eng/facts",
		"/lenses/eng/facts?q=x",
		"/lenses/eng/facts?repo=zulu",
	)
}
