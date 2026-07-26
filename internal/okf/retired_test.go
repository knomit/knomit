package okf

import (
	"path"
	"strings"
	"testing"
	"time"
)

func TestRenderRetired(t *testing.T) {
	ts := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	retired := []Retirement{
		{Date: ts, Title: "Kimi K2 pricing", Path: "kb/technology/ai/old/aaaa1111.md",
			Kind: "superseded", SuccessorPath: "kb/technology/ai/new/bbbb2222.md"},
		{Date: ts.AddDate(0, -1, 0), Title: "Stale guidance", Path: "kb/technology/ai/x/cccc3333.md",
			Kind: "retracted"},
	}
	resolve := func(p string) (FactRef, bool) {
		if p == "kb/technology/ai/new/bbbb2222.md" {
			return FactRef{Path: "kb/technology/ai/new/kimi-k3-bbbb2222.md", Title: "Kimi K3 profile"}, true
		}
		return FactRef{}, false
	}

	got := string(renderRetired(retired, RenderOpts{ResolveFact: resolve}))

	if !strings.Contains(got, "type: Retired Knowledge") {
		t.Errorf("not a conformant concept:\n%s", got)
	}
	// A superseded fact links its replacement, by title.
	if !strings.Contains(got, "**superseded** Kimi K2 pricing → [Kimi K3 profile](../kb/technology/ai/new/kimi-k3-bbbb2222.md)") {
		t.Errorf("superseded entry wrong:\n%s", got)
	}
	// A retracted fact has no successor and must not fabricate one.
	if !strings.Contains(got, "**retracted** Stale guidance\n") {
		t.Errorf("retracted entry wrong:\n%s", got)
	}
	// Grouped newest-first with a month jump bar, like the digests.
	if !strings.Contains(got, "**Months:** [2026-07](#2026-07)") {
		t.Errorf("missing month jump bar:\n%s", got)
	}
	// Counts are stated so a reader sees the shape at a glance.
	if !strings.Contains(got, "1 superseded") || !strings.Contains(got, "1 retracted") {
		t.Errorf("missing counts:\n%s", got)
	}
}

func TestRenderRetired_EmptyRendersNothing(t *testing.T) {
	if got := renderRetired(nil, RenderOpts{}); got != nil {
		t.Errorf("no retirements should produce no document, got:\n%s", got)
	}
}

// A successor that is itself retired has no document in the bundle. Its title
// is still known (it is in the retirement list), so the entry names it plainly
// rather than emitting a link to a document that does not exist.
func TestRenderRetired_UnresolvableSuccessorIsNotLinked(t *testing.T) {
	ts := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	retired := []Retirement{
		{Date: ts, Title: "Older claim", Path: "kb/a/b/aaaa1111.md",
			Kind: "superseded", SuccessorPath: "kb/a/b/bbbb2222.md"},
		{Date: ts, Title: "Also withdrawn", Path: "kb/a/b/bbbb2222.md", Kind: "retracted"},
	}
	got := string(renderRetired(retired, RenderOpts{}))

	if !strings.Contains(got, "**superseded** Older claim → Also withdrawn\n") {
		t.Errorf("unresolved successor should be named, not linked:\n%s", got)
	}
	if strings.Contains(got, "](") && strings.Contains(got, "bbbb2222") {
		t.Errorf("must not link a document the bundle does not contain:\n%s", got)
	}
}

// Newest first, and stable within a day by title then path — the same ordering
// contract the digests hold to, so the view is byte-deterministic.
func TestRenderRetired_DeterministicOrdering(t *testing.T) {
	day := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	older := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	retired := []Retirement{
		{Date: older, Title: "Oldest", Path: "kb/a/b/3.md", Kind: "retracted"},
		{Date: day, Title: "Bravo", Path: "kb/a/b/2.md", Kind: "retracted"},
		{Date: day, Title: "Alpha", Path: "kb/a/b/1.md", Kind: "retracted"},
	}
	got := string(renderRetired(retired, RenderOpts{}))

	iAlpha := strings.Index(got, "Alpha")
	iBravo := strings.Index(got, "Bravo")
	iOldest := strings.Index(got, "Oldest")
	if !(iAlpha < iBravo && iBravo < iOldest) {
		t.Errorf("ordering wrong (want Alpha < Bravo < Oldest):\n%s", got)
	}
	if !strings.Contains(got, "## 2026-07") || !strings.Contains(got, "### 2026-07-15") {
		t.Errorf("missing month/day sections:\n%s", got)
	}
	// Input order must not matter.
	shuffled := []Retirement{retired[2], retired[0], retired[1]}
	if other := string(renderRetired(shuffled, RenderOpts{})); other != got {
		t.Errorf("render is not order-independent")
	}
}

// The load-bearing constraint: a withdrawn fact reaches the bundle as an index
// entry and NOTHING else. No concept document is emitted for it — not even a
// deprecated one — because a conformant consumer may ignore optional
// frontmatter and would ingest the disavowed claim as current.
func TestBuild_RetiredFactGetsNoConceptDocument(t *testing.T) {
	ts := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	live := factInput(t, "kb/decisions/okf/scope/d9d6557d.md", `---
kind: epistemic
type: principle
domain: [okf]
---
# Export scope is repo only

Body.`, ts)

	b, skips := Build(RepoIdentity{ID: "0123456789ab"}, []FactInput{live}, nil, RenderOpts{
		Retired: []Retirement{{
			Date: ts, Title: "Withdrawn claim about pricing",
			Path: "kb/technology/ai/old/aaaa1111.md",
			Kind: "superseded", SuccessorPath: "kb/decisions/okf/scope/d9d6557d.md",
		}},
	})
	if skips.Skipped != 0 {
		t.Fatalf("unexpected skips: %+v", skips)
	}
	if err := Validate(b); err != nil {
		t.Fatalf("bundle not conformant: %v", err)
	}
	m := bundleMap(b)

	doc, ok := m["views/retired.md"]
	if !ok {
		t.Fatalf("views/retired.md missing")
	}
	// The successor is live, so it is linked forward from the index.
	if !strings.Contains(doc, "**superseded** Withdrawn claim about pricing → [Export scope is repo only](") {
		t.Errorf("successor not linked from the index:\n%s", doc)
	}
	// No concept document anywhere carries the retired fact's title or its uuid.
	//
	// The exemptions are the files whose subject IS the withdrawal: views/retired.md
	// and the changelogs, which name the title inside a row that says it was
	// withdrawn. That records the retirement rather than re-asserting the claim —
	// the thing this test exists to prevent is an INGESTIBLE document, and neither
	// a reserved log nor the retired index is one.
	for p, content := range m {
		if p == "views/retired.md" || p == "views/index.md" || path.Base(p) == "log.md" {
			continue
		}
		if strings.Contains(content, "Withdrawn claim about pricing") || strings.Contains(content, "aaaa1111") {
			t.Errorf("retired fact leaked into %s:\n%s", p, content)
		}
	}
	// The live fact is untouched: no "supersedes" backreference anywhere.
	for p, content := range m {
		if strings.Contains(strings.ToLower(content), "supersedes") {
			t.Errorf("live document %s gained a supersedes line:\n%s", p, content)
		}
	}
	// views/index.md lists it beside the digests.
	if idx := m["views/index.md"]; !strings.Contains(idx, "[retired](retired.md)") {
		t.Errorf("views/index.md does not list the retired view:\n%s", idx)
	}
}
