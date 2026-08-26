package synthesize

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// structuralEnv builds a corpus with a realistic token distribution plus ONE
// designated duplicate pair, so a test can say which route found it.
//
// The filler matters: both rarity cuts are read off the corpus's own
// distribution, so a corpus with no distribution has nothing rare in it. Every
// filler fact carries a common year token and a unique widget number, which is
// what an ordinary corpus's identifier vocabulary looks like.
//
// 404 facts, because shortlistBudget is corpus-scaled at 5 per 1000 and the
// reservation this commit adds only exists at budget >= 2. A 200-fact corpus
// budgets 1 and the reservation test would pass vacuously.
const structuralFiller = 404

func structuralEnv(t *testing.T, pair [2]struct{ Path, Title string }) *restatementEnv {
	t.Helper()
	env := newRestatementEnv(t, 0)
	for i := range structuralFiller {
		env.writeFact(
			fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1),
			fmt.Sprintf("Filler note 2026 about widget %d", 1000+i),
			"an unrelated body")
	}
	for _, f := range pair {
		env.writeFact(f.Path, f.Title, "a body about the event")
	}
	return env
}

func namedFact(path, title string) struct{ Path, Title string } {
	return struct{ Path, Title string }{path, title}
}

// pairKindIn returns the stored match kind for a pair, or "" if it never stood.
func pairKindIn(t *testing.T, env *restatementEnv, a, b string) string {
	t.Helper()
	pairs, err := env.svc.Abstraction().RestatementPairsByRank(context.Background(), env.branch, 100_000)
	require.NoError(t, err)
	for _, p := range pairs {
		if (p.APath == a && p.BPath == b) || (p.APath == b && p.BPath == a) {
			return p.MatchKind
		}
	}
	return ""
}

// TestStructural_PathPrefixExtension — vulnerabilities/cisco and
// vulnerabilities/networking/cisco are one subject filed at two depths. One
// token set contains the other, and the containment rests on `cisco` rather
// than on the prefix every security fact shares.
//
// Measured pair: 1e8287a2 / bfbb31b8, verified identical by body comparison
// (the same nine CVEs), title cosine 0.917 — and absent from a 14,768-row
// standing cache.
func TestStructural_PathPrefixExtension(t *testing.T) {
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchPathIdentity, pairKindIn(t, env, a, b),
		"one path's tokens inside the other's is one subject filed at two depths")
}

// TestStructural_PathSegmentInversion — devops/gitlab and gitlab/devops are the
// same filing decision made in two orders. Freeform categories produce this
// whenever the same event is ingested twice.
func TestStructural_PathSegmentInversion(t *testing.T) {
	a := "kb/technology/devops/gitlab/aaaaaaa1.md"
	b := "kb/technology/gitlab/devops/bbbbbbb2.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "GitLab directive flaw exploited within days"),
		namedFact(b, "Attackers reach GitLab through a directive flaw"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchPathIdentity, pairKindIn(t, env, a, b),
		"the same segments in a different order are the same identity")
}

// TestStructural_PluralFold — vulnerabilities/gitlab against
// vulnerability/gitlab. The singular/plural collapse arrives from textnorm
// rather than from anything invented here, which is why it is asserted rather
// than reimplemented.
//
// CASE is deliberately not exercised through a path: WriteFact lowercases fact
// paths, so a mixed-case path never reaches the corpus and a test that wrote
// one would be asserting against a path the store does not hold. (The first
// draft of this test did exactly that and passed nothing — the fact simply was
// not there under the name it looked for.) Case folding still matters for
// TITLES, which is where identifierTokens reads it, and
// TestStructural_RareIdentifierToken covers that path.
func TestStructural_PluralFold(t *testing.T) {
	a := "kb/technology/security/vulnerabilities/gitlab/aaaaaaa3.md"
	b := "kb/technology/security/vulnerability/gitlab/bbbbbbb4.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "GitLab honeypot traffic climbs after disclosure"),
		namedFact(b, "Honeypots record GitLab exploitation attempts"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchPathIdentity, pairKindIn(t, env, a, b),
		"plural is spelling, not identity")
}

// TestStructural_SlugVersusUUIDFilename — a generated uuid filename is minted
// per fact and can therefore never match anything, so it is dropped before the
// paths are compared. An authored slug is content and stays.
func TestStructural_SlugVersusUUIDFilename(t *testing.T) {
	a := "kb/technology/security/vulnerabilities/gitlab/check-point-advisory.md"
	b := "kb/technology/security/vulnerabilities/gitlab/aaaaaaa5.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "GitLab advisory reaches customers late"),
		namedFact(b, "Late customer notification on the GitLab advisory"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchPathIdentity, pairKindIn(t, env, a, b),
		"a uuid filename must not block a match its directory already makes")
}

// TestStructural_RareIdentifierToken — the generalisation of "extract the CVE
// key". The two facts sit in different category sub-trees, so no path rule
// reaches them; what they share is an identifier-SHAPED token that is rare in
// this corpus.
//
// Measured pair: check-point-smartconsole-cve-2026-16232 / 23d98f38, same CVE,
// same CVSS, body cosine 0.930, absent from the standing cache.
func TestStructural_RareIdentifierToken(t *testing.T) {
	a := "kb/technology/security/vulnerabilities/network-security/check-point-smartconsole.md"
	b := "kb/technology/security/vulnerabilities/network-appliances/aaaaaaa6.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "SmartConsole authentication bypass CVE-2026-16232 under active exploitation"),
		namedFact(b, "Check Point flaw CVE-2026-16232 exploited against a handful of customers"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchRareToken, pairKindIn(t, env, a, b),
		"a shared rare identifier is evidence of identity that no path rule reaches")
}

// TestStructural_CommonIdentifierTokenIsNotEvidence is the other half, and it
// is what makes the rarity test mean something.
//
// Both facts carry `2026`, which every filler fact also carries. A rule with a
// hard-coded pattern — `CVE-\d+`, or "any number" — would pair them; the rule
// here asks the corpus how specific the token is, and the corpus says it is
// the least specific token it has.
func TestStructural_CommonIdentifierTokenIsNotEvidence(t *testing.T) {
	a := "kb/technology/hardware/storage/aaaaaaa7.md"
	b := "kb/technology/policy/procurement/aaaaaaa8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Storage shipments fell in 2026"),
		namedFact(b, "Procurement rules changed in 2026"),
	})
	env.seedShortlist()
	require.Equal(t, "", pairKindIn(t, env, a, b),
		"a token the whole corpus carries says nothing about identity")
}

// TestStructural_SharedGenericPrefixIsNotIdentity — every security fact's path
// contains the same top segments. Containment alone would therefore pair a
// corpus with itself; the match has to rest on a token more specific than the
// corpus's typical one.
func TestStructural_SharedGenericPrefixIsNotIdentity(t *testing.T) {
	a := "kb/technology/filler/aaaaaaa9.md"
	b := "kb/technology/filler/topic3/bbbbbbbc.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "One note about a widget"),
		namedFact(b, "Another note about a different widget"),
	})
	env.seedShortlist()
	require.Equal(t, "", pairKindIn(t, env, a, b),
		"the prefix the whole corpus shares is not an identity claim")
}

// TestStructural_ReachesTheJudge is the wiring half, and the point of the
// commit: detection that never reaches the judge is this issue's own failure
// mode arriving one layer later.
//
// The two facts' titles are far apart on the axis — that is why the KNN never
// paired them — so they cannot be selected through the cosine ranking. Only the
// reserved structural slot gets them judged.
func TestStructural_ReachesTheJudge(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})

	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, reviewTool, env.branch, "")
	require.NoError(t, err)
	require.NoError(t, planRestatementShortlist(ctx, d, sess, env.branch, nil))

	// The fixture must actually budget more than one slot, or the reservation
	// under test does not exist.
	require.GreaterOrEqual(t, shortlistBudget(structuralFiller+2), 2,
		"fixture must fund a budget of at least two, or the reservation cannot fire")

	require.True(t, shortlistQueueHoldsPair(t, env, sess.ID, a, b),
		"a structurally matched pair must be offered to the judge, not merely detected")
}

// TestStructural_ReservationSurvivesAFullOrdinaryBand — the reservation is only
// meaningful when the ordinary band could have taken every slot. A widener that
// fires only on underfill is decorative (the designer's Q10 ruling on the motif
// signal, which this reservation copies).
func TestStructural_ReservationSurvivesAFullOrdinaryBand(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	env.seedShortlist()

	d := env.deps()
	budget := shortlistBudget(structuralFiller + 2)
	ordinary, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, budget*shortlistOverfetch)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ordinary), budget,
		"fixture must supply enough title-ranked pairs to fill the budget alone, "+
			"or the reservation is not being tested")

	pairs, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, structuralFiller+2)
	require.NoError(t, err)
	require.Equal(t, 1, h.StructuralOffered,
		"the reserved slot must be spent even when the ordinary band could have filled the budget")
	require.True(t, containsPair(pairs, a, b))
	require.Len(t, pairs, budget, "the reservation reallocates a slot, it does not add one")
}

// TestStructural_NoTitleVectorIsDroppedNotScored — the ranking column means
// "title cosine". A structural pair whose vectors are not both stored is
// dropped rather than given a placeholder, because a placeholder there is a
// number no measurement produced. The pair is not lost: the axis backfill fills
// in over sessions and a later refresh mints it.
//
// The fixture needs a PARTIALLY filled axis, and getting there is the whole
// difficulty. An empty axis does not exercise this at all — with no fact on the
// axis the refresh has nothing to rescan and returns before any pair is built,
// so a test written that way passes without ever reaching the code it names.
// (It was written that way first, and the sabotage that invents a score walked
// straight through it.) So: one twin is written first and lands in the single
// batch the budget affords, the other is written last and does not.
func TestStructural_NoTitleVectorIsDroppedNotScored(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"

	emb := &restatementEmbedder{perBatchDelay: 40 * time.Millisecond}
	env := newRestatementEnvWith(t, 0, emb)
	env.writeFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload", "body")
	for i := range titleBackfillBatch * 2 {
		env.writeFact(fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1),
			fmt.Sprintf("Filler note 2026 about widget %d", 1000+i), "an unrelated body")
	}
	env.writeFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload", "body")

	// One batch fits in the budget; the rest of the corpus does not.
	d := env.deps()
	have, total, err := ensureTitleVectors(ctx, d, env.branch, time.Millisecond)
	require.NoError(t, err)
	require.Greater(t, have, 0, "fixture needs SOME facts on the axis")
	require.Less(t, have, total,
		"fixture needs a PARTIAL axis, or the drop under test is never reached")

	onAxis, err := env.svc.Abstraction().LiveEpistemicFactsOnAxis(ctx, env.branch)
	require.NoError(t, err)
	byPath := map[string]bool{}
	for _, p := range onAxis {
		byPath[p] = true
	}
	require.True(t, byPath[a], "fixture: the first twin must be on the axis")
	require.False(t, byPath[b], "fixture: the second twin must NOT be on the axis")

	_, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)
	require.Equal(t, "", pairKindIn(t, env, a, b),
		"a pair that cannot be scored on the title axis must not be stored with an invented score")

	// ...and once the axis closes, the same corpus yields the pair.
	env.seedShortlist()
	require.Equal(t, store.MatchPathIdentity, pairKindIn(t, env, a, b),
		"the pair is deferred by a missing vector, never discarded")
}

// TestStructural_ContainmentWithoutSpecificityIsNotPathIdentity pins what
// classify MEANS, as opposed to what the inverted index happens to reach.
//
// These two facts are reached through their shared rare identifier, and their
// path token sets are in a containment relation — but the containment rests
// only on the prefix the whole corpus shares, so it is not an identity claim
// and the pair must be reported as what it actually is.
//
// This is the only test that reaches pathIdentity with a pair the index did not
// select FOR its path tokens; without it, dropping the specificity requirement
// inside pathIdentity changes nothing any test observes, because the index
// enforces the same rule on the way in.
func TestStructural_ContainmentWithoutSpecificityIsNotPathIdentity(t *testing.T) {
	a := "kb/technology/filler/aaaaaab1.md"
	b := "kb/technology/filler/deeper/aaaaaab2.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "One report on CVE-2026-77777"),
		namedFact(b, "A second report on CVE-2026-77777"),
	})
	env.seedShortlist()
	require.Equal(t, store.MatchRareToken, pairKindIn(t, env, a, b),
		"containment on the corpus's shared prefix is not a path identity — "+
			"the shared identifier is what found this pair, and the record must say so")
}
