package fact

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

const mvPath = "kb/notes/f.md"

// mv builds a serialized fact version for the merge tests.
type mv struct {
	title, body string
	typ         Type
	conf        float64
	sources     int
	domain      []string
	entities    []string
	motifs      []string
	refs        []string
	expires     string
}

func (v mv) bytes(t testing.TB) []byte {
	t.Helper()
	f := NewFact(mvPath)
	f.Title, f.Body = v.title, v.body
	f.Kind = Epistemic
	f.Type = v.typ
	if f.Type == "" {
		f.Type = Observation
	}
	f.Confidence, f.Sources = v.conf, v.sources
	f.Domain, f.Entities, f.Motifs, f.Refs = v.domain, v.entities, v.motifs, v.refs
	if f.Domain == nil {
		f.Domain = []string{}
	}
	if f.Entities == nil {
		f.Entities = []string{}
	}
	if f.Refs == nil {
		f.Refs = []string{}
	}
	f.Expires = v.expires
	s, err := SerializeFact(f)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return []byte(s)
}

func baseVersion() mv {
	return mv{title: "Base title", body: "Base body.", conf: 0.7, sources: 1,
		domain: []string{"store"}, entities: []string{"A"}, refs: []string{"kb/notes/r.md"}}
}

func mustMerge(t *testing.T, base, src, dst []byte, st MergeStrategy) (Fact, MergeRecord, []byte) {
	t.Helper()
	out, rec, ok := MergeVersions(mvPath, base, src, dst, st)
	if !ok {
		t.Fatalf("MergeVersions not ok: %q", rec.Reason)
	}
	f, err := ParseFact(mvPath, string(out))
	if err != nil {
		t.Fatalf("merged output does not parse: %v\n%s", err, out)
	}
	return f, rec, out
}

// T1: src changed the body, dst LOWERED the confidence. Each change was made
// by one side only, so both survive and the strategy decides nothing.
//
// SABOTAGE: taking max(confidence) (dedup's rule) instead of the per-field
// rule keeps the base's 0.7 → red; letting the strategy decide every field
// (skipping the one-side arm) loses one of the two changes → red.
func TestMergeVersions_OneSideChanged_NoStrategy(t *testing.T) {
	b := baseVersion()
	s := b
	s.body = "src rewrote the body."
	d := b
	d.conf = 0.4
	f, rec, _ := mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{})
	if f.Body != s.body {
		t.Fatalf("body = %q, want src's %q", f.Body, s.body)
	}
	if f.Confidence != 0.4 {
		t.Fatalf("confidence = %v, want dst's lowered 0.4", f.Confidence)
	}
	if f.Sources != 1 {
		t.Fatalf("sources = %d, want the unchanged 1 (never a sum)", f.Sources)
	}
	if len(rec.Decided) != 0 {
		t.Fatalf("Decided = %v, want none: the three-way rule alone produced this", rec.Decided)
	}
}

// T2: both sides rewrote the body. The more confident version's body is taken
// WHOLE, never spliced, and the record says the strategy decided it.
//
// SABOTAGE: swapping the confidence comparison → the less confident body →
// red; a body built from both sides → not byte-equal to either → red.
func TestMergeVersions_BothChangedBody_ConfidenceWins(t *testing.T) {
	b := baseVersion()
	s := b
	s.body = "src's rewrite, line one.\n\nsrc line two."
	s.conf = 0.9
	d := b
	d.body = "dst's rewrite."
	d.title = "Dst retitled"
	for _, order := range []string{"src wins", "dst wins"} {
		src, dst := s, d
		if order == "dst wins" {
			src, dst = d, s
		}
		f, rec, _ := mustMerge(t, b.bytes(t), src.bytes(t), dst.bytes(t), MergeStrategy{})
		if f.Body != s.body {
			t.Fatalf("%s: body = %q, want the confident version's whole body %q", order, f.Body, s.body)
		}
		if f.Confidence != 0.9 {
			t.Fatalf("%s: confidence = %v, want 0.9 (one side changed it)", order, f.Confidence)
		}
		if f.Title != "Dst retitled" {
			t.Fatalf("%s: title = %q: only one side changed it, so it survives", order, f.Title)
		}
		if strings.Join(rec.Decided, ",") != MergeFieldBody {
			t.Fatalf("%s: Decided = %v, want [body]", order, rec.Decided)
		}
	}
}

// The confidence rule's tie-breaks, in order: sources, then non-hypothesis,
// then the smaller blob hash.
func TestMergeVersions_ConfidenceTieBreaks(t *testing.T) {
	b := baseVersion()
	s, d := b, b
	s.body, d.body = "src body", "dst body"
	s.sources, d.sources = 3, 2
	f, _, _ := mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{})
	if f.Body != "src body" {
		t.Fatalf("equal confidence: more sources must win, got %q", f.Body)
	}
	s.sources, d.sources = 2, 2
	s.typ = Hypothesis
	f, _, _ = mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{})
	if f.Body != "dst body" {
		t.Fatalf("equal confidence and sources: the non-hypothesis must win, got %q", f.Body)
	}
	s.typ = Observation
	sb, db := s.bytes(t), d.bytes(t)
	want := "src body"
	if GitBlobHash(db) < GitBlobHash(sb) {
		want = "dst body"
	}
	f, _, _ = mustMerge(t, b.bytes(t), sb, db, MergeStrategy{})
	if f.Body != want {
		t.Fatalf("exact tie: the smaller blob hash must win, got %q want %q", f.Body, want)
	}
}

// T3: the merge does not depend on which argument a version arrived in. The
// peer merges (base, consensus, own) and the host (base, peer, own): the same
// two versions in opposite slots must give the same bytes, or the pair never
// settles. 200 generated triples, many of them exact ties.
//
// SABOTAGE: a tie-break that favours src (dedup's `sources >=`), or lists
// ordered src-first instead of winner-first → red.
func TestMergeVersions_Symmetric(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	pick := func(xs ...string) []string {
		var out []string
		for _, x := range xs {
			if r.Intn(2) == 0 {
				out = append(out, x)
			}
		}
		return out
	}
	gen := func() mv {
		v := mv{
			title:    []string{"T one", "T two", "T three"}[r.Intn(3)],
			body:     []string{"b1", "b2", "b3"}[r.Intn(3)],
			conf:     []float64{0.5, 0.7, 0.9}[r.Intn(3)],
			sources:  1 + r.Intn(2),
			domain:   pick("store", "merge", "sync", "fact"),
			entities: pick("A", "B", "C", "D"),
			motifs:   pick("merge-base-moves", "ping-pong-merge", "silent-side-pick"),
			refs:     pick("kb/notes/r.md", "kb/notes/q.md", "https://example.com/x"),
		}
		if r.Intn(4) == 0 {
			v.typ = Hypothesis
		}
		return v
	}
	for i := 0; i < 200; i++ {
		b, x, y := gen(), gen(), gen()
		var base []byte
		if i%10 != 0 { // every tenth triple has no base (add/add)
			base = b.bytes(t)
		}
		xb, yb := x.bytes(t), y.bytes(t)
		o1, r1, ok1 := MergeVersions(mvPath, base, xb, yb, MergeStrategy{})
		o2, r2, ok2 := MergeVersions(mvPath, base, yb, xb, MergeStrategy{})
		if ok1 != ok2 || string(o1) != string(o2) {
			t.Fatalf("triple %d: Merge(b,x,y) != Merge(b,y,x)\nok %v/%v (%q/%q)\n--- x,y\n%s\n--- y,x\n%s", i, ok1, ok2, r1.Reason, r2.Reason, o1, o2)
		}
		// upstream: the same version is upstream in both calls.
		u1, _, _ := MergeVersions(mvPath, base, xb, yb, MergeStrategy{Rule: MergeUpstream, Upstream: MergeSrc})
		u2, _, _ := MergeVersions(mvPath, base, yb, xb, MergeStrategy{Rule: MergeUpstream, Upstream: MergeDst})
		if string(u1) != string(u2) {
			t.Fatalf("triple %d: upstream merge depends on the argument order\n%s\n---\n%s", i, u1, u2)
		}
	}
}

// The upstream rule takes the consensus side's version of a field both
// changed, whatever the confidences say, and nothing else.
func TestMergeVersions_Upstream(t *testing.T) {
	b := baseVersion()
	s, d := b, b
	s.body, s.conf = "consensus body", 0.3
	d.body, d.conf = "own body", 0.95
	d.entities = []string{"A", "Z"}
	f, rec, _ := mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{Rule: MergeUpstream, Upstream: MergeSrc})
	if f.Body != "consensus body" || f.Confidence != 0.3 {
		t.Fatalf("upstream=src: got body %q conf %v, want the consensus side's", f.Body, f.Confidence)
	}
	if strings.Join(f.Entities, ",") != "A,Z" {
		t.Fatalf("a list only dst changed survives: %v", f.Entities)
	}
	if rec.Strategy != "upstream" || rec.Winner != MergeSrc {
		t.Fatalf("record = %+v", rec)
	}
	if _, rec, ok := MergeVersions(mvPath, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{Rule: MergeUpstream}); ok || rec.Reason != "bad-strategy" {
		t.Fatalf("upstream without a side must refuse, got ok=%v %q", ok, rec.Reason)
	}
}

// T4: lists merge three-way. src REMOVED ref R and added domain "sync"; dst
// ADDED ref Q and removed entity B. A blind union (dedup's, which has no base)
// would put R and B back.
//
// SABOTAGE: blind union (ignore removals) → R and B present → red.
func TestMergeVersions_Lists_ThreeWay(t *testing.T) {
	b := baseVersion()
	b.refs = []string{"kb/notes/r.md", "kb/notes/keep.md"}
	b.entities = []string{"A", "B"}
	s := b
	s.refs = []string{"kb/notes/keep.md"}
	s.domain = []string{"store", "sync"}
	s.body = "src body" // both sides also changed the body, so no list is one-side-only by accident
	s.conf = 0.9
	d := b
	d.refs = []string{"kb/notes/r.md", "kb/notes/keep.md", "kb/notes/q.md", mvPath}
	d.entities = []string{"A"}
	d.body = "dst body"
	d.domain = []string{"store", "fact"}
	f, _, _ := mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{})
	if got := strings.Join(f.Refs, ","); got != "kb/notes/keep.md,kb/notes/q.md" {
		t.Fatalf("refs = %s: R (removed by src) must be gone, Q (added by dst) kept, the self-ref dropped", got)
	}
	if got := strings.Join(f.Entities, ","); got != "A" {
		t.Fatalf("entities = %s: B (removed by dst) must be gone", got)
	}
	if got := strings.Join(f.Domain, ","); got != "store,sync,fact" {
		t.Fatalf("domain = %s: both additions, the winner's (src) first", got)
	}
}

// T5: what cannot be merged is refused with a reason, and the caller falls
// back to its site's side-pick: an unparsable version, a kind mismatch.
//
// SABOTAGE: returning ok=true for an unparsable side or a kind mismatch → red.
func TestMergeVersions_NotAFact_FallsBack(t *testing.T) {
	b := baseVersion()
	good := b
	good.body = "edited"
	pol := b
	cases := []struct {
		name     string
		src, dst []byte
		reason   string
	}{
		{"unparsable src", []byte("not a fact\n"), good.bytes(t), "src-unparsable"},
		{"unparsable dst", good.bytes(t), []byte("---\ntype: observation\n---\nno heading\n"), "dst-unparsable"},
		{"kind mismatch", good.bytes(t), func() []byte {
			f := NewFact(mvPath)
			f.Title, f.Body, f.Kind, f.Type, f.Confidence, f.Sources = pol.title, "x", Pragmatic, Policy, 0.7, 1
			f.Domain, f.Entities, f.Refs = []string{}, []string{}, []string{}
			s, err := SerializeFact(f)
			if err != nil {
				t.Fatal(err)
			}
			return []byte(s)
		}(), "kind-mismatch"},
		{"ontology yaml is not a fact", []byte("id: x\nname: X\n"), []byte("id: y\nname: Y\n"), "src-unparsable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, rec, ok := MergeVersions(mvPath, b.bytes(t), c.src, c.dst, MergeStrategy{})
			if ok || out != nil {
				t.Fatalf("ok=%v: %s must not merge", ok, c.name)
			}
			if rec.Reason != c.reason {
				t.Fatalf("Reason = %q, want %q", rec.Reason, c.reason)
			}
		})
	}
}

// Add/add: no base. Fields that agree are kept, fields that differ go to the
// strategy, lists union. Never a panic.
//
// SABOTAGE: treating a nil base as "unchanged" (take dst for every field) →
// the confident src body is lost → red.
func TestMergeVersions_AddAdd_NoBase(t *testing.T) {
	s := baseVersion()
	s.body, s.conf, s.entities = "src added", 0.9, []string{"A", "S"}
	d := baseVersion()
	d.body, d.entities = "dst added", []string{"A", "D"}
	f, rec, _ := mustMerge(t, nil, s.bytes(t), d.bytes(t), MergeStrategy{})
	if f.Body != "src added" || f.Confidence != 0.9 {
		t.Fatalf("got body %q conf %v, want the confident src's", f.Body, f.Confidence)
	}
	if got := strings.Join(f.Entities, ","); got != "A,S,D" {
		t.Fatalf("entities = %s, want the union winner-first", got)
	}
	if got := strings.Join(rec.Decided, ","); got != "body,confidence" {
		t.Fatalf("Decided = %s", got)
	}
}

// P5: merging must never drop what a side wrote. A version with a frontmatter
// key this build does not read, an expiry ParseFact drops, or a motif it drops
// is not merged: the caller keeps its side-pick, which moves bytes whole.
//
// SABOTAGE: skipping parseLossless's checks (plain ParseFact) → ok=true and the
// unknown key / bad expiry vanishes from the output → red.
func TestMergeVersions_Lossless(t *testing.T) {
	b := baseVersion()
	d := b
	d.body = "dst edit"
	src := string(b.bytes(t))
	inject := func(line string) []byte {
		return []byte(strings.Replace(src, "---\n# ", line+"\n---\n# ", 1))
	}
	cases := map[string][]byte{
		"unknown key":   inject("reviewed_by: alice"),
		"bad expires":   inject("expires: tomorrow"),
		"dropped motif": inject("motifs: [NotKebab]"),
		"bad ref shape": []byte(strings.Replace(src, "refs: [kb/notes/r.md]", "refs: [kb/notes/r.md, 'src://nope']", 1)),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFact(mvPath, string(s)); err != nil {
				t.Fatalf("fixture must still parse (the loss is silent): %v", err)
			}
			out, rec, ok := MergeVersions(mvPath, b.bytes(t), s, d.bytes(t), MergeStrategy{})
			if ok {
				t.Fatalf("a lossy version merged:\n%s", out)
			}
			if rec.Reason != "src-lossy" {
				t.Fatalf("Reason = %q, want src-lossy", rec.Reason)
			}
		})
	}
	// An expiry written with an offset is the same instant in Z: not a loss.
	z := b
	z.expires = "2027-01-01T00:00:00Z"
	withOffset := []byte(strings.Replace(string(z.bytes(t)), "2027-01-01T00:00:00Z", "2027-01-01T02:00:00+02:00", 1))
	if _, rec, ok := MergeVersions(mvPath, b.bytes(t), withOffset, d.bytes(t), MergeStrategy{}); !ok {
		t.Fatalf("an offset expiry is not a loss: %q", rec.Reason)
	}
}

// Motifs are capped: a union past MaxMotifs is cut winner-first and the cut is
// recorded as decided.
func TestMergeVersions_MotifCap(t *testing.T) {
	b := baseVersion()
	s, d := b, b
	s.motifs = []string{"merge-base-moves", "ping-pong-merge"}
	s.conf = 0.9
	d.motifs = []string{"silent-side-pick", "union-biased-merge"}
	f, rec, _ := mustMerge(t, b.bytes(t), s.bytes(t), d.bytes(t), MergeStrategy{})
	if got := strings.Join(f.Motifs, ","); got != "merge-base-moves,ping-pong-merge,silent-side-pick" {
		t.Fatalf("motifs = %s", got)
	}
	if fmt.Sprint(rec.Decided) != "[motifs]" {
		t.Fatalf("Decided = %v, want [motifs]", rec.Decided)
	}
}
