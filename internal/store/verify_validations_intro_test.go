package store

import (
	"bytes"
	"testing"

	"knomit/internal/store/verify"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// A net-change refusal names the commit that WROTE the failing content, not
// the branch's tip: the commit a fix or a drop has to deal with. Here the
// bad note is followed by a good commit on another file; the refusal names
// the bad commit.
//
// SABOTAGE: name the tip in verify.BranchFactsValid's refusal (as the first net
// version did) -> the refusal names the good commit -> red.
func TestValidation_TheRefusalNamesTheCommitThatWroteTheFact(t *testing.T) {
	h := newPushHostWith(t, map[string]string{fact.OntologyFile: validatedOntology("")}, 0)
	p := newPeer(t, h.srv.URL)
	bad := p.commit(p.head(), nil, "kb/notes/bad.md", note("bad"))
	good := p.commit(bad, nil, "kb/notes/good.md", note("good", "e"))
	p.setBranch("work", good)
	err := p.push(h.srv.URL, &bytes.Buffer{}, "+refs/heads/work:refs/heads/"+peerBranch)
	require.Equal(t, verify.RuleValidation, pushRefusal(t, err))
	require.Contains(t, err.Error(), "commit "+bad.Hash.String()+" "+verify.CommitRefusedByValidation)
	require.NotContains(t, err.Error(), good.Hash.String())
}
