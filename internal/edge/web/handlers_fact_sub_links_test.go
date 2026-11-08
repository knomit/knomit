package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/httpx"
)

// A retracted entry of "cited by" / "cites" links to a commit where its fact
// EXISTS, so the reader can follow it (user ruling 2026-11-08: the deleted mark
// is the fact's state NOW; the link is how it is still followed).
//
// The citer kb/m-old.md is written at m1 citing kb/t.md, then MOVED to
// kb/m-new.md at m2 (same bytes, so the same blob: its reference edges stay
// those resolved at m1), then retracted at m3. The cited kb/c.md is retracted
// at m4. Incoming of kb/t.md lists the citing blob under BOTH paths, each at a
// commit where THAT path holds it: kb/m-old.md at m1, kb/m-new.md at m2 (not
// m1, where kb/m-new.md does not exist). Outgoing of kb/t.md lists kb/c.md at
// its target_commit c1. All three are retracted, and all three link.
//
// Every link is then FOLLOWED: the fact read at it answers 200 without
// ?fallback, so each link names a commit where its fact exists.
// Sabotage: restore the pre-fix `!ref.Deleted` gate and the retracted items
// have no link.
func TestRefLinks_RetractedEntriesLinkWhereTheFactExists(t *testing.T) {
	ctx := context.Background()
	m, svcs := newRealStoreManager(t, "alpha")
	svc := svcs["alpha"]
	s := &Server{Manager: m}
	c := map[string]string{}
	const br = fsAgentBranch

	write := func(step, path, title string, mod func(*fact.Fact)) {
		t.Helper()
		res, err := svc.Facts().WriteFact(ctx, br, path, factText(t, path, title, 0.9, mod), step+": "+path, "learn")
		require.NoError(t, err)
		c[step] = res.CommitHash
	}
	batch := func(step string, files map[string]string, deletes ...string) map[string]string {
		t.Helper()
		h, blobs, err := svc.Facts().BatchWriteFacts(ctx, br, files, deletes, step, "retract")
		require.NoError(t, err)
		c[step] = h
		return blobs
	}

	write("c1", "kb/c.md", "Cited", refsTo(fact.Principle))
	write("t1", "kb/t.md", "Target", refsTo(fact.Observation, "kb/c.md"))
	citer := factText(t, "kb/m-old.md", "Citer", 0.9, refsTo(fact.Concept, "kb/t.md"))
	blobsOld := batch("m1", map[string]string{"kb/m-old.md": citer})
	blobsNew := batch("m2", map[string]string{"kb/m-new.md": citer}, "kb/m-old.md")
	require.Equal(t, blobsOld["kb/m-old.md"], blobsNew["kb/m-new.md"], "the move keeps the blob")
	batch("m3", nil, "kb/m-new.md")
	batch("m4", nil, "kb/c.md")

	base := httpx.APIBase + "/repos/alpha/branches/agent:test"
	in := getRefs(t, s, "/repos/alpha/branches/agent:test/facts/kb/t.md/incoming")
	got := map[string]string{}
	for _, r := range in.Embedded.Refs {
		require.True(t, r.Deleted, "%s is retracted", r.Path)
		require.Contains(t, r.Links, "self", "%s: a retracted entry still links", r.Path)
		got[r.Path] = r.Links["self"].Href
	}
	require.Equal(t, map[string]string{
		"kb/m-old.md": base + "/commits/" + c["m1"] + "/facts/kb/m-old.md",
		"kb/m-new.md": base + "/commits/" + c["m2"] + "/facts/kb/m-new.md",
	}, got)

	out := getRefs(t, s, "/repos/alpha/branches/agent:test/facts/kb/t.md/outgoing")
	require.Len(t, out.Embedded.Refs, 1)
	o := out.Embedded.Refs[0]
	require.True(t, o.Deleted)
	require.Equal(t, c["c1"], o.Commit)
	require.Equal(t, base+"/commits/"+c["c1"]+"/facts/kb/c.md", o.Links["self"].Href)
	got["kb/c.md"] = o.Links["self"].Href

	// The commit-anchored route links the same way.
	anch := getRefs(t, s, "/repos/alpha/branches/agent:test/commits/"+c["t1"]+"/facts/kb/t.md/outgoing")
	require.Equal(t, o.Links["self"].Href, anch.Embedded.Refs[0].Links["self"].Href)

	// Following each link reads the fact (no fallback needed).
	for path, href := range got {
		rec := getAPI(t, s, href[len(httpx.APIBase):])
		require.Equal(t, http.StatusOK, rec.Code, "%s at %s: %s", path, href, rec.Body.String())
	}
}
