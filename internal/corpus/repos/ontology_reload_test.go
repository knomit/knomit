package repos

// knomit/knomit#429: an ontology change that arrives through git is applied
// live, after the sync round that brings it, with no restart. The rule
// (reloadOntology, stages.go): consensus is honoured IN FULL — added and
// removed topics, every attribute — and only an ontology that cannot be used
// at all (missing, empty, unparseable, a symlink, or the reserved fleet id on
// a repo that is not the fleet repository) is not applied; then the ontology
// in force stays, and the repo's status says why.
//
// Real transport, real rounds: a bare origin advanced with git itself
// (ghOrigin, conflicts_matrix_test.go), one instance cloning it, and
// OriginRoundForTest running exactly the Sync stage's round.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

const reloadBase = `id: live
name: Live
topics:
  alpha:
    description: alpha
  beta:
    description: beta
`

// commit commits files (path -> content) on the origin's consensus branch,
// the way a merged PR lands there, and pushes it.
func (o *ghOrigin) commit(t *testing.T, msg string, files map[string]string) {
	t.Helper()
	runGit(t, o.work, "fetch", "origin")
	runGit(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	for p, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(o.work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(o.work, p), []byte(content), 0o644))
	}
	runGit(t, o.work, "add", "-A")
	runGit(t, o.work, "commit", "-m", msg)
	runGit(t, o.work, "push", "origin", o.branch)
}

func newReloadFixture(t *testing.T) (*ghOrigin, *ghInstance) {
	t.Helper()
	root := t.TempDir()
	o := newGHOrigin(t, root, "main", reloadBase, map[string]string{})
	g := newGHInstance(t, root, o, "agent/aaaa1111", "instance-a")
	require.NotNil(t, g.ri.Ontology(), "the clone mounts with the origin's ontology")
	return o, g
}

func syncRound(t *testing.T, g *ghInstance) {
	t.Helper()
	require.NoError(t, g.ri.OriginRoundForTest(context.Background()))
}

func placementOK(ri *RepoInstance, topic string) error {
	_, err := fact.ValidatePlacement(ri.Ontology(), topic, "x/y")
	return err
}

// The tip's ontology blob on the read branch, as git names it.
func readTipBlob(t *testing.T, ri *RepoInstance) (commit, blob string) {
	t.Helper()
	c, _, b, err := testService(t, ri).OntologyBlobAt(context.Background(), ri.ReadBranch())
	require.NoError(t, err)
	return c, b
}

// An ADDED topic synced from the origin is accepted for writes without a
// restart. SABOTAGE: drop the reload from afterFetch → still refused → red.
func TestOntologyReload_AddedTopicIsWritableAfterSync(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	require.Error(t, placementOK(g.ri, "gamma"), "gamma is not a topic yet")

	o.commit(t, "add gamma", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	syncRound(t, g)

	require.NoError(t, placementOK(g.ri, "gamma"), "the synced topic is accepted with no restart")
	require.True(t, g.ri.WritableBranch(g.branch))
	_, blob := readTipBlob(t, g.ri)
	st, ok := g.ri.OntologyStatus()
	require.True(t, ok)
	require.Equal(t, blob, st.Blob, "status names the blob in force: the synced one")
	require.Equal(t, OntologyPath, st.Path)
	require.Empty(t, st.ReloadError)
}

// A REMOVED topic is refused after the sync: consensus is honoured in full,
// not only when it adds (no subset or additive-only filter).
func TestOntologyReload_RemovedTopicIsRefusedAfterSync(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	require.NoError(t, placementOK(g.ri, "beta"))

	o.commit(t, "drop beta", map[string]string{OntologyPath: "id: live\nname: Live\ntopics:\n  alpha:\n    description: alpha\n"})
	syncRound(t, g)

	require.Error(t, placementOK(g.ri, "beta"), "a topic consensus removed is no longer writable")
	require.NoError(t, placementOK(g.ri, "alpha"))
}

// A CHANGED attribute is honoured after the sync: a topic attribute
// (learn_dedup) and a root attribute (verify_signatures) alike — the
// boot preset refresh's no-attribute-change rule does not apply here.
func TestOntologyReload_ChangedAttributesAreHonoured(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	require.False(t, g.ri.Ontology().LearnDedupOff("alpha"))
	_, has := g.ri.Ontology().Attributes[fact.AttrVerifySignatures]
	require.False(t, has)

	o.commit(t, "dedup off, verify log", map[string]string{OntologyPath: "id: live\nname: Live\n" +
		"attributes:\n  verify_signatures: log\n" +
		"topics:\n  alpha:\n    description: alpha\n    attributes:\n      learn_dedup: off\n  beta:\n    description: beta\n"})
	syncRound(t, g)

	require.True(t, g.ri.Ontology().LearnDedupOff("alpha"), "learn_dedup: off is in force")
	require.Equal(t, "log", g.ri.Ontology().Attributes[fact.AttrVerifySignatures], "verify_signatures arrives by consensus and is in force")
}

// An UNPARSEABLE ontology at the new tip keeps the ontology in force, keeps
// the repo writable, and reports the error in status; a later usable one is
// applied and clears it.
func TestOntologyReload_UnparseableKeepsTheOldAndReports(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	st0, _ := g.ri.OntologyStatus()

	o.commit(t, "break it", map[string]string{OntologyPath: "topics: [::\n"})
	syncRound(t, g)

	require.Same(t, before, g.ri.Ontology(), "an unusable ontology is not applied")
	require.True(t, g.ri.WritableBranch(g.branch), "the repo stays writable on the ontology in force")
	tipCommit, tipBlob := readTipBlob(t, g.ri)
	st, ok := g.ri.OntologyStatus()
	require.True(t, ok)
	require.Equal(t, st0.Blob, st.Blob, "status still names the blob in force")
	require.Contains(t, st.ReloadError, "does not parse")
	require.Equal(t, tipCommit, st.ReloadCommit)
	require.Equal(t, tipBlob, st.ReloadBlob)

	// Another round on the same broken blob changes nothing.
	syncRound(t, g)
	require.Same(t, before, g.ri.Ontology())

	o.commit(t, "fix it", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	syncRound(t, g)
	require.NoError(t, placementOK(g.ri, "gamma"))
	st, _ = g.ri.OntologyStatus()
	require.Empty(t, st.ReloadError, "the fixed ontology is in force and the error is gone")
}

// An EMPTY ontology at the tip is unusable too.
func TestOntologyReload_EmptyKeepsTheOld(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	o.commit(t, "empty it", map[string]string{OntologyPath: ""})
	syncRound(t, g)
	require.Same(t, before, g.ri.Ontology())
	st, _ := g.ri.OntologyStatus()
	require.Contains(t, st.ReloadError, "is empty")
}

// A SYMLINKED ontology arriving by sync keeps the ontology in force and is
// reported; the running repo is not turned Unavailable (its next mount
// refuses it, as Identify always has).
func TestOntologyReload_SymlinkKeepsTheOldAndReports(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	runGit(t, o.work, "fetch", "origin")
	runGit(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	require.NoError(t, os.WriteFile(filepath.Join(o.work, "other.yaml"), []byte(reloadBase), 0o644))
	p := filepath.Join(o.work, OntologyPath)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink("../other.yaml", p))
	runGit(t, o.work, "add", "-A")
	runGit(t, o.work, "commit", "-m", "symlink it")
	runGit(t, o.work, "push", "origin", o.branch)
	syncRound(t, g)

	require.Same(t, before, g.ri.Ontology())
	require.True(t, g.ri.WritableBranch(g.branch))
	st, _ := g.ri.OntologyStatus()
	require.Contains(t, st.ReloadError, "symlink")
	require.Equal(t, OntologyPath, st.ReloadPath)
}

// The reserved fleet id arriving on a repo that is not the fleet repository
// is not applied: it would make the repo IsFleet and hand it to the fleet
// code. It is reported like an unusable file.
func TestOntologyReload_FleetIDOnANonFleetRepoIsNotApplied(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	o.commit(t, "become the fleet", map[string]string{OntologyPath: "id: fleet\nname: Fleet\ntopics:\n  alpha:\n    description: alpha\n"})
	syncRound(t, g)

	require.Same(t, before, g.ri.Ontology())
	require.False(t, g.ri.IsFleet())
	st, _ := g.ri.OntologyStatus()
	require.Contains(t, st.ReloadError, ErrFleetOntologyReserved.Error())
}

// An UNCHANGED ontology blob does no re-parse: a round that brings other
// commits leaves the very same ontology value in force, and status still
// names the commit it was read at, not the new tip.
func TestOntologyReload_UnchangedBlobDoesNotReparse(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	st0, _ := g.ri.OntologyStatus()

	o.commit(t, "unrelated", map[string]string{"notes.txt": "hello\n"})
	syncRound(t, g)

	require.Same(t, before, g.ri.Ontology(), "same blob: the parsed ontology is kept, not re-parsed")
	tipCommit, tipBlob := readTipBlob(t, g.ri)
	st, _ := g.ri.OntologyStatus()
	require.Equal(t, st0, st, "status is unchanged")
	require.Equal(t, tipBlob, st.Blob)
	require.NotEqual(t, tipCommit, st.Commit, "the read branch moved; the ontology was not re-read")
}

// A repo with NO usable ontology that then receives one becomes writable.
// (A clone with no ontology is refused at create, so the unusable starting
// state is installed by hand; the reload is what the test exercises.)
func TestOntologyReload_NoOntologyThenOneBecomesWritable(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	g.ri.ontMu.Lock()
	cur := g.ri.ident.Load()
	g.ri.ident.Store(&identity{ontologyErr: errors.New("ontology at .knomit/ontology.yaml does not parse"),
		src: ontologySource{Commit: cur.src.Commit, Path: cur.src.Path, Blob: "0000000000000000000000000000000000000000"}})
	g.ri.ontMu.Unlock()
	require.False(t, g.ri.WritableBranch(g.branch))

	o.commit(t, "a usable ontology", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	syncRound(t, g)

	require.NoError(t, g.ri.OntologyError())
	require.True(t, g.ri.WritableBranch(g.branch), "a usable ontology makes the repo writable")
	require.NoError(t, placementOK(g.ri, "gamma"))
}

// The swap is one atomic store: readers running through reloads always see
// a whole ontology (run with -race).
func TestOntologyReload_ConcurrentReadersSeeOneOntology(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ont := g.ri.Ontology()
				require.NotNil(t, ont)
				_ = ont.TopicNames()
				_, _ = g.ri.OntologyStatus()
				_ = g.ri.WritableBranch(g.branch)
			}
		}()
	}
	o.commit(t, "add gamma", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	syncRound(t, g)
	o.commit(t, "add delta", map[string]string{OntologyPath: reloadBase + "  delta:\n    description: delta\n"})
	syncRound(t, g)
	close(stop)
	wg.Wait()
	require.NoError(t, placementOK(g.ri, "delta"))
	require.Error(t, placementOK(g.ri, "gamma"))
}

// The commit observer is the catch-all trigger: a read branch moved by
// anything other than the Sync stage's round — here the store's own Sync,
// called directly, which no afterFetch follows — still has its ontology
// applied, one debounce later. SABOTAGE: drop the reload from the observer
// callback (stages.go openStage) → never applied → red.
func TestOntologyReload_CommitObserverAppliesItWithoutARound(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	o.commit(t, "add gamma", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	_, err := testService(t, g.ri).Remote().Sync(context.Background(), g.branch, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return placementOK(g.ri, "gamma") == nil }, 10*time.Second, 50*time.Millisecond,
		"the observer applies the merged ontology")
}
