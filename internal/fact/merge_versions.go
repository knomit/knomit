package fact

import (
	"crypto/sha1" //nolint:gosec // a git blob id, not a security digest
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MergeRule is how MergeVersions settles a field BOTH versions changed
// differently. A field only one side changed never reaches it.
type MergeRule string

const (
	// MergeConfidence is dedup's winner rule made symmetric: the version with
	// the higher confidence, then more sources, then the one that is not a
	// hypothesis, then the smaller git blob hash of the version's bytes. The
	// last step means nothing and is fixed, which is all determinism needs.
	MergeConfidence MergeRule = "confidence"
	// MergeUpstream takes the consensus branch's version (MergeStrategy.Upstream
	// names which argument that is): "remote wins", limited to fields both
	// sides changed.
	MergeUpstream MergeRule = "upstream"
)

// MergeSide names one of MergeVersions' two versions by its argument.
type MergeSide string

const (
	MergeSrc MergeSide = "src"
	MergeDst MergeSide = "dst"
)

// MergeStrategy is the both-changed rule plus, for MergeUpstream, which
// argument is the consensus side (src on a peer merging the consensus branch
// in, dst on the host merging a peer's branch into its own).
type MergeStrategy struct {
	Rule     MergeRule // empty reads as MergeConfidence
	Upstream MergeSide // required by MergeUpstream
}

// Name is the rule as the Knomit-Merge trailer records it.
func (s MergeStrategy) Name() string {
	if s.Rule == "" {
		return string(MergeConfidence)
	}
	return string(s.Rule)
}

// The field names MergeRecord.Decided uses, in the fixed order it lists them.
const (
	MergeFieldTitle          = "title"
	MergeFieldBody           = "body"
	MergeFieldType           = "type" // kind, type and origin: they validate together
	MergeFieldDomain         = "domain"
	MergeFieldConfidence     = "confidence"
	MergeFieldSources        = "sources"
	MergeFieldEntities       = "entities"
	MergeFieldMotifs         = "motifs"
	MergeFieldRefs           = "refs"
	MergeFieldEvidenceWeight = "evidence_weight"
	MergeFieldExpires        = "expires"
)

// MergeRecord is what one MergeVersions call did, for the merge commit's
// Knomit-Merge trailer (ok) or Knomit-Conflict trailer (not ok).
type MergeRecord struct {
	Strategy string    // MergeStrategy.Name()
	Winner   MergeSide // the side the strategy names; it decides every field in Decided
	// Decided lists the fields the strategy chose because both sides changed
	// them differently (plus motifs when the cap cut the union). Empty means
	// the three-way rule alone produced the result.
	Decided []string
	// Reason says why ok is false: not-a-fact paths are the caller's call, so
	// this is one of "unparsable", "lossy", "kind-mismatch", "not-serializable"
	// or "bad-strategy", prefixed by the version it concerns where there is one.
	Reason string
}

// MergeVersions merges two versions of ONE fact against their common
// ancestor, field by field — the git-merge-time counterpart of dedup, which
// merges two DIFFERENT facts and so has no base to consult.
//
// For every field: neither side changed it → base; one side changed it →
// that side; both changed it to the same value → that value; both changed it
// differently → the strategy's winner, recorded in rec.Decided. Title and body
// are taken WHOLE from one side, never spliced. Lists (domain, entities,
// motifs, refs) merge as sets against the base, so each side's additions AND
// removals survive; the winner's order comes first, then the loser's
// additions. Sources and confidence follow the field rule, never sum or max:
// both versions already count the base's corroborations, and a side that
// deliberately lowered one made a change to keep.
//
// base == nil means the path has no base (both sides added it): every field
// that differs is the strategy's, and lists union.
//
// ok is false — and the caller keeps its site's side-picking behaviour — when
// a version does not parse, the two kinds differ, a version would lose
// something on the way through ParseFact and SerializeFact (a frontmatter key
// this build does not know, a ref/motif/expires ParseFact drops, a subject
// motif SerializeFact strips), or the merged fact would not write back exactly.
// Merging must never drop what a side wrote.
//
// Pure: a function of the three blobs and the strategy only (no clock, no
// embedding, no local state), so the two instances of a conflict compute the
// same bytes whichever of them merges.
func MergeVersions(path string, base, src, dst []byte, strategy MergeStrategy) (out []byte, rec MergeRecord, ok bool) {
	rec.Strategy = strategy.Name()
	switch strategy.Rule {
	case "", MergeConfidence:
	case MergeUpstream:
		if strategy.Upstream != MergeSrc && strategy.Upstream != MergeDst {
			rec.Reason = "bad-strategy"
			return nil, rec, false
		}
	default:
		rec.Reason = "bad-strategy"
		return nil, rec, false
	}

	s, why := parseLossless(path, src)
	if why != "" {
		rec.Reason = "src-" + why
		return nil, rec, false
	}
	d, why := parseLossless(path, dst)
	if why != "" {
		rec.Reason = "dst-" + why
		return nil, rec, false
	}
	var b Fact
	hasBase := base != nil
	if hasBase {
		var err error
		if b, err = ParseFact(path, string(base)); err != nil {
			rec.Reason = "base-unparsable"
			return nil, rec, false
		}
	}
	if s.Kind != d.Kind {
		rec.Reason = "kind-mismatch"
		return nil, rec, false
	}

	rec.Winner = mergeWinner(s, d, src, dst, strategy)
	srcWins := rec.Winner == MergeSrc
	decide := func(field string, baseEqSrc, baseEqDst, srcEqDst bool) MergeSide {
		side, byRule := chooseSide(hasBase, baseEqSrc, baseEqDst, srcEqDst, rec.Winner)
		if byRule {
			rec.Decided = append(rec.Decided, field)
		}
		return side
	}
	pick := func(side MergeSide) *Fact {
		if side == MergeSrc {
			return &s
		}
		return &d
	}

	m := NewFact(path)
	m.Title = pick(decide(MergeFieldTitle, b.Title == s.Title, b.Title == d.Title, s.Title == d.Title)).Title
	m.Body = pick(decide(MergeFieldBody, b.Body == s.Body, b.Body == d.Body, s.Body == d.Body)).Body
	t := pick(decide(MergeFieldType, sameType(b, s), sameType(b, d), sameType(s, d)))
	m.Kind, m.Type, m.Origin = t.Kind, t.Type, t.Origin
	m.Domain = mergeList(hasBase, b.Domain, s.Domain, d.Domain, srcWins)
	m.Confidence = pick(decide(MergeFieldConfidence, b.Confidence == s.Confidence, b.Confidence == d.Confidence, s.Confidence == d.Confidence)).Confidence
	m.Sources = pick(decide(MergeFieldSources, b.Sources == s.Sources, b.Sources == d.Sources, s.Sources == d.Sources)).Sources
	m.Entities = mergeList(hasBase, b.Entities, s.Entities, d.Entities, srcWins)
	m.Motifs = mergeList(hasBase, b.Motifs, s.Motifs, d.Motifs, srcWins)
	if len(m.Motifs) > MaxMotifs {
		m.Motifs = m.Motifs[:MaxMotifs]
		rec.Decided = append(rec.Decided, MergeFieldMotifs)
	}
	if len(m.Motifs) == 0 {
		m.Motifs = nil // absent and empty are one thing (Fact.Motifs)
	}
	m.Refs = dropSelfRef(mergeList(hasBase, b.Refs, s.Refs, d.Refs, srcWins), path)
	m.EvidenceWeight = pick(decide(MergeFieldEvidenceWeight, b.EvidenceWeight == s.EvidenceWeight, b.EvidenceWeight == d.EvidenceWeight, s.EvidenceWeight == d.EvidenceWeight)).EvidenceWeight
	be, se, de := normExpires(b.Expires), normExpires(s.Expires), normExpires(d.Expires)
	m.Expires = pick(decide(MergeFieldExpires, be == se, be == de, se == de)).Expires
	if m.Domain == nil {
		m.Domain = []string{}
	}
	if m.Entities == nil {
		m.Entities = []string{}
	}
	if m.Refs == nil {
		m.Refs = []string{}
	}

	text, err := SerializeFact(m)
	if err != nil {
		rec.Reason = "not-serializable"
		return nil, rec, false
	}
	back, err := ParseFact(path, text)
	if err != nil || !sameFact(m, back) {
		rec.Reason = "not-serializable"
		return nil, rec, false
	}
	return []byte(text), rec, true
}

// chooseSide is the per-field three-way rule. byRule is true only when both
// sides changed the field to different values and the strategy's winner was
// taken. Without a base every difference is such a change.
func chooseSide(hasBase, baseEqSrc, baseEqDst, srcEqDst bool, winner MergeSide) (side MergeSide, byRule bool) {
	switch {
	case srcEqDst:
		return MergeSrc, false // neither changed it, or both made the same change
	case hasBase && baseEqSrc:
		return MergeDst, false // only dst changed it
	case hasBase && baseEqDst:
		return MergeSrc, false // only src changed it
	default:
		return winner, true
	}
}

// mergeWinner is the strategy's choice between the two whole versions. It is
// a total order on (fact, bytes), independent of which argument a version
// arrived in, so both instances of a conflict name the same version.
func mergeWinner(s, d Fact, sb, db []byte, st MergeStrategy) MergeSide {
	if st.Rule == MergeUpstream {
		return st.Upstream
	}
	switch {
	case s.Confidence != d.Confidence:
		if s.Confidence > d.Confidence {
			return MergeSrc
		}
		return MergeDst
	case s.Sources != d.Sources:
		if s.Sources > d.Sources {
			return MergeSrc
		}
		return MergeDst
	}
	sHyp, dHyp := s.Type == Hypothesis, d.Type == Hypothesis
	if sHyp != dHyp {
		if dHyp {
			return MergeSrc
		}
		return MergeDst
	}
	if GitBlobHash(sb) <= GitBlobHash(db) {
		return MergeSrc
	}
	return MergeDst
}

// GitBlobHash is git's object id for data as a blob: the hex SHA-1 of
// "blob <len>\x00<data>". It is the tie-break of last resort in
// MergeConfidence and the id the Knomit-Merge trailer names.
func GitBlobHash(data []byte) string {
	h := sha1.New() //nolint:gosec // git's object id, not a security digest
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// mergeList is the three-way set merge: an element survives when a side has
// it and neither side removed it from the base. When only one side changed
// the list it is taken as that side wrote it, order and all. Otherwise the
// winner's order comes first, then the loser's additions — so the bytes do not
// depend on which argument a version arrived in.
func mergeList(hasBase bool, base, src, dst []string, srcWins bool) []string {
	switch {
	case equalStrings(src, dst):
		return src
	case hasBase && equalStrings(base, src):
		return dst
	case hasBase && equalStrings(base, dst):
		return src
	}
	inBase := setOf(base)
	inSrc, inDst := setOf(src), setOf(dst)
	removed := func(x string) bool {
		return hasBase && inBase[x] && (!inSrc[x] || !inDst[x])
	}
	winner, loser := src, dst
	if !srcWins {
		winner, loser = dst, src
	}
	var out []string
	seen := map[string]bool{}
	for _, list := range [][]string{winner, loser} {
		for _, x := range list {
			if seen[x] || removed(x) {
				continue
			}
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func dropSelfRef(refs []string, path string) []string {
	self := strings.ToLower(path)
	out := refs[:0:0]
	for _, r := range refs {
		if strings.ToLower(r) == self {
			continue
		}
		out = append(out, r)
	}
	return out
}

func setOf(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// equalStrings treats nil and empty as equal: ParseFact leaves an absent
// motifs list nil and every other list empty.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameType(a, b Fact) bool {
	return a.Kind == b.Kind && a.Type == b.Type && a.Origin == b.Origin
}

// normExpires compares expiries by instant, not spelling: SerializeFact
// writes every expiry in UTC, so "+02:00" and its Z form are one value.
func normExpires(s string) string {
	if n, err := NormalizeExpires(s); err == nil {
		return n
	}
	return s
}

// sameFact compares everything a fact file stores.
func sameFact(a, b Fact) bool {
	return a.path == b.path && a.Title == b.Title && a.Body == b.Body && sameType(a, b) &&
		a.Confidence == b.Confidence && a.Sources == b.Sources && a.EvidenceWeight == b.EvidenceWeight &&
		normExpires(a.Expires) == normExpires(b.Expires) &&
		equalStrings(a.Domain, b.Domain) && equalStrings(a.Entities, b.Entities) &&
		equalStrings(a.Motifs, b.Motifs) && equalStrings(a.Refs, b.Refs)
}

// knownFrontmatterKeys are the keys the frontmatter struct reads; anything
// else in a version would vanish when the merge writes the fact back.
var knownFrontmatterKeys = map[string]bool{
	"kind": true, "type": true, "domain": true, "confidence": true, "sources": true,
	"entities": true, "motifs": true, "refs": true, "evidence_weight": true,
	"origin": true, "expires": true,
}

// parseLossless parses one version and reports why it cannot be merged
// without losing something ("" when it can): "unparsable", or "lossy" when
// ParseFact dropped or would drop part of it, or SerializeFact would not write
// back exactly what ParseFact read.
func parseLossless(path string, data []byte) (Fact, string) {
	content := string(data)
	f, err := ParseFact(path, content)
	if err != nil {
		return Fact{}, "unparsable"
	}
	if len(f.RefWarnings)+len(f.MotifWarnings)+len(f.ExpiresWarnings) > 0 {
		return Fact{}, "lossy"
	}
	if !knownKeysOnly(content) {
		return Fact{}, "lossy"
	}
	text, err := SerializeFact(f)
	if err != nil {
		return Fact{}, "lossy"
	}
	back, err := ParseFact(path, text)
	if err != nil || !sameFact(f, back) {
		return Fact{}, "lossy"
	}
	return f, ""
}

// knownKeysOnly reports whether every frontmatter key is one ParseFact reads.
// The block is located exactly as ParseFact locates it.
func knownKeysOnly(content string) bool {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	rest := strings.TrimPrefix(content, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return false
	}
	var keys map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(rest[:end]), &keys); err != nil {
		return false
	}
	for k := range keys {
		if !knownFrontmatterKeys[k] {
			return false
		}
	}
	return true
}
