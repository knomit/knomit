package repos

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestVerifyAccepts_Scope: an accept naming a repository matches only there;
// one with no repository matches in any.
func TestVerifyAccepts_Scope(t *testing.T) {
	m := newLifecycleManagerWithRoot(t, t.TempDir())
	a := m.VerifyAccepts()
	require.NotNil(t, a)
	global := plumbing.NewHash("1111111111111111111111111111111111111111")
	scoped := plumbing.NewHash("2222222222222222222222222222222222222222")
	require.NoError(t, a.Add(global, "", "pre-signing"))
	require.NoError(t, a.Add(scoped, "uid-a", "criss-cross"))

	for _, c := range []struct {
		uid    string
		hash   plumbing.Hash
		want   bool
		wantIn string
	}{
		{"uid-a", global, true, ""},
		{"uid-b", global, true, ""},
		{"uid-a", scoped, true, "uid-a"},
		{"uid-b", scoped, false, ""},
	} {
		got, ok := a.For(c.uid).Lookup(c.hash)
		require.Equal(t, c.want, ok, "uid=%s hash=%s", c.uid, c.hash.String()[:4])
		if ok {
			require.Equal(t, c.wantIn, got.RepoUID)
		}
	}
	list, err := a.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
}

// TestVerifyAccepts_UnblockAnE4Refusal: the E4 remedy end to end. A remote
// agent branch holding an unsigned (pre-signing) commit of this machine is
// refused at create; after `knomit verify accept` (Add, no repo: the repo does
// not exist yet), the same create adopts it.
func TestVerifyAccepts_UnblockAnE4Refusal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := newLifecycleManagerWithRoot(t, root)
	bare := filepath.Join(root, "remote.git")
	url := seedBareRemoteNoOntology(t, bare)
	work := t.TempDir()
	runGit(t, "", "clone", bareOf(url), work)
	runGit(t, work, "checkout", "-b", m.deps.AgentBranch)
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "init: create knowledge base (pre-signing)")
	runGit(t, work, "push", "origin", m.deps.AgentBranch)
	unsigned := strings.TrimSpace(gitOut(t, bare, "rev-parse", m.deps.AgentBranch))

	_, err = m.Create(context.Background(), CreateSpec{
		Name: "rejoined", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, func(Event) {})
	require.ErrorIs(t, err, store.ErrForeignLineage, "the pre-signing commit is refused, loudly")
	require.Contains(t, err.Error(), "knomit verify accept "+unsigned, "the error names the exact command")
	require.Equal(t, unsigned, strings.TrimSpace(gitOut(t, bare, "rev-parse", m.deps.AgentBranch)), "the remote is untouched")

	require.NoError(t, m.VerifyAccepts().Add(plumbing.NewHash(unsigned), "", "mine, from before signing"))
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "rejoined", Mode: "clone", Origin: &OriginSpec{URL: url, Branch: "main"},
	}, func(Event) {})
	require.NoError(t, err, "accepted on this instance, the lineage is adopted")
	var head string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		h, herr := svc.Branches().HeadCommit(context.Background(), m.deps.AgentBranch)
		require.NoError(t, herr)
		head = h
	}))
	require.Equal(t, unsigned, head)
}
