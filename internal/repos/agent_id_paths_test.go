package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/testsupport/testsigner"
)

// pathsAgentID is the persisted agent id every authored commit must name. It
// differs from what the test fixture fallback derives from tmplAgent
// ("test-abc"), so a creation path that forgot SetAgentID authors as the
// fallback and is caught. Without this, the fallback hides the missing
// plumbing: production would fail that path with store.ErrNoAgentID.
const pathsAgentID = "opaque-1"

func pathsManager(t *testing.T, root string) *Manager {
	t.Helper()
	require.NotEqual(t, pathsAgentID, testsigner.FixtureAgentID(tmplAgent), "fixture: the id must differ from the fallback's")
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: root},
		AgentBranch: tmplAgent,
		AgentID:     pathsAgentID,
		Signer:      testsigner.Named("paths-host"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// writeAndAuthor writes one fact on ri's agent branch through the store the
// stages opened and returns the head commit's author e-mail.
func writeAndAuthor(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	svc := testService(t, ri)
	ctx := context.Background()
	_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/notes/paths.md",
		"---\ntype: observation\nconfidence: 0.9\n---\n# p\n\nbody\n", "learn: p", "learn")
	require.NoError(t, err)
	tip, err := svc.HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)
	info, err := svc.FactsRepo().CommitInfo(ctx, plumbing.NewHash(tip))
	require.NoError(t, err)
	return info.AuthorEmail
}

// D18/P1b review F2: every authored creation path names the PERSISTED agent
// id, Deps.AgentID. The template and initialize rows assert the commit the
// CREATION path itself authors (initTemplate's template commit,
// initInitialize's ontology commit), so dropping SetAgentID from either turns
// its row red; the local and clone rows assert the first authored write
// after create, through the store the stages open.
//
// SABOTAGE: drop svc.SetAgentID(m.deps.AgentID) in initTemplate → the
// template row reads "test-abc+…" → red (shown in the PR report); likewise
// in initInitialize → the initialize row; in stages.go → local and clone.
func TestCreatePaths_AuthorAsThePersistedAgentID(t *testing.T) {
	want := pathsAgentID + "@agents.knomit.io"
	for _, tc := range []struct {
		name   string
		author func(t *testing.T, m *Manager, root string) string
	}{
		{"local", func(t *testing.T, m *Manager, root string) string {
			ri, err := m.Create(context.Background(), CreateSpec{Name: "loc", Mode: "preset"}, nil)
			require.NoError(t, err)
			return writeAndAuthor(t, ri)
		}},
		{"template", func(t *testing.T, m *Manager, root string) string {
			playbooksSource(t, m, root, "clone")
			ri, err := m.Create(context.Background(), CreateSpec{Name: "tm", Mode: "template",
				Template: &TemplateRef{Repo: "playbooks", Name: "mission"}}, nil)
			require.NoError(t, err)
			dir := cloneBranch(t, ri, tmplAgent)
			return gitIn(t, dir, "log", "-1", "--format=%ae", "--grep=^Knomit-Template: mission$", tmplAgent)
		}},
		{"clone", func(t *testing.T, m *Manager, root string) string {
			url := sourceRepo(t, root, "remote", sourceSpec{})
			ri := mount(t, m, "cl", "clone", url)
			return writeAndAuthor(t, ri)
		}},
		{"initialize", func(t *testing.T, m *Manager, root string) string {
			url := seedBareRemoteNoOntology(t, filepath.Join(root, "init.git"))
			_, err := m.Create(context.Background(), CreateSpec{Name: "in", Mode: "initialize", OntologyPreset: "code",
				Origin: &OriginSpec{URL: url}}, nil)
			require.NoError(t, err)
			// The ontology commit initInitialize authored and pushed.
			return gitIn(t, bareOf(url), "log", "-1", "--format=%ae", tmplAgent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			m := pathsManager(t, root)
			got := tc.author(t, m, root)
			require.NotEmpty(t, got, "fixture: the path authored a commit")
			// author is <id>+<operation>@agents.knomit.io
			require.Regexp(t, `^`+pathsAgentID+`\+[a-z-]+@agents\.knomit\.io$`, got, "author of the %s path (want %s with an operation)", tc.name, want)
		})
	}
}

// TestManagerAgentID_NoneGivenReportsNone: a Manager built without
// Deps.AgentID has no agent id, in a test binary as in production. It must not
// derive one from the agent branch's name or from the fixture signer.
func TestManagerAgentID_NoneGivenReportsNone(t *testing.T) {
	m := New(context.Background(), Deps{AgentBranch: tmplAgent})
	require.Equal(t, "", m.AgentID())
}
