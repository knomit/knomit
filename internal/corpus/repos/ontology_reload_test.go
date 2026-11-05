package repos

// knomit/knomit#429: an ontology change that arrives through git is applied
// live, after the sync round that brings it, with no restart. The rule
// (reloadOntology, stages.go): consensus is honoured IN FULL — added and
// removed topics, every attribute. An ontology that cannot be used at all
// (missing, empty, unparseable, a symlink) makes the repo UNWRITABLE until
// consensus fixes it (decision 1); the reserved fleet id on a repo that is
// not the fleet repository makes it UNAVAILABLE, fleet_conflict (decision
// 2). A read error is neither: it changes nothing and is retried.
//
// Real transport, real rounds: a bare origin advanced with git itself
// (ghOrigin, conflicts_matrix_test.go), one instance cloning it, and
// OriginRoundForTest running exactly the Sync stage's round.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
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
		// Removed first, so a path that is a symlink is REPLACED by a file
		// rather than written through.
		_ = os.Remove(filepath.Join(o.work, p))
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
	require.Empty(t, st.Error)
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

// countReads installs a hook on g's ontology file reads: it counts every
// read, and fails the next `fail` of them with a transient error.
func countReads(g *ghInstance, fail int32) *atomic.Int32 {
	var n atomic.Int32
	var left atomic.Int32
	left.Store(fail)
	h := func() error {
		n.Add(1)
		if left.Add(-1) >= 0 {
			return errors.New("injected transient read failure")
		}
		return nil
	}
	g.ri.ontologyReadHook.Store(&h)
	return &n
}

// A TRANSIENT READ ERROR is "unknown, ask again", never "no ontology": it is
// not recorded as a refused file, so the next round applies the usable
// consensus ontology. SABOTAGE: readOntology returning the read error as a
// plain "no ontology" error with src = the tip → stored as reloadErr with
// reloadSrc = the tip's blob, the refused-blob skip keeps it out → red.
func TestOntologyReload_TransientReadErrorRetriesAndApplies(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	before := g.ri.Ontology()
	// The observer may also try during the window; fail generously, then
	// stop failing before the second round.
	o.commit(t, "add gamma", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	countReads(g, 1000)
	syncRound(t, g)
	require.Same(t, before, g.ri.Ontology(), "an unreadable file is not applied")
	st, _ := g.ri.OntologyStatus()
	require.Empty(t, st.Error, "a read error is not a verdict on the file and is not reported as one")
	require.True(t, g.ri.WritableBranch(g.branch), "a read error never makes a working repo unwritable")
	require.NoError(t, g.ri.OntologyError())

	countReads(g, 0)
	syncRound(t, g)
	require.NoError(t, placementOK(g.ri, "gamma"), "the next trigger reads it and applies it")
}

// The same on a repo with NO usable ontology in force: a read error must not
// become its OntologyError naming the tip's file (src), or sameFile would
// skip that file for good.
func TestOntologyReload_TransientReadErrorOnARepoWithNoOntology(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	orig := errors.New("ontology at .knomit/ontology.yaml does not parse")
	g.ri.ontMu.Lock()
	cur := g.ri.ident.Load()
	g.ri.ident.Store(&identity{ontologyErr: orig, src: cur.src})
	g.ri.ontMu.Unlock()

	o.commit(t, "a usable ontology", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	countReads(g, 1000)
	syncRound(t, g)
	require.ErrorIs(t, g.ri.OntologyError(), orig, "nothing is recorded for an unreadable file")
	st, _ := g.ri.OntologyStatus()
	require.Equal(t, cur.src, ontologySource{Commit: st.Commit, Path: st.Path, Blob: st.Blob}, "src does not move to the unread tip")

	countReads(g, 0)
	syncRound(t, g)
	require.NoError(t, g.ri.OntologyError())
	require.True(t, g.ri.WritableBranch(g.branch))
}

// remove deletes path on the origin's consensus branch.
func (o *ghOrigin) remove(t *testing.T, msg, path string) {
	t.Helper()
	runGit(t, o.work, "fetch", "origin")
	runGit(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	runGit(t, o.work, "rm", "-q", path)
	runGit(t, o.work, "commit", "-m", msg)
	runGit(t, o.work, "push", "origin", o.branch)
}

// symlink replaces path on the origin's consensus branch with a symlink to a
// file whose TEXT is itself a usable ontology, so a reader that followed it
// would apply it.
func (o *ghOrigin) symlink(t *testing.T, path string) {
	t.Helper()
	runGit(t, o.work, "fetch", "origin")
	runGit(t, o.work, "checkout", "-B", o.branch, "origin/"+o.branch)
	require.NoError(t, os.WriteFile(filepath.Join(o.work, "other.yaml"), []byte(reloadBase), 0o644))
	p := filepath.Join(o.work, path)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink("../other.yaml", p))
	runGit(t, o.work, "add", "-A")
	runGit(t, o.work, "commit", "-m", "symlink it")
	runGit(t, o.work, "push", "origin", o.branch)
}

// writeFactAttempt is an in-process fact write straight through the store,
// below every authored-write gate: the review pipeline, a trigger and the
// fleet code write this way.
func writeFactAttempt(t *testing.T, g *ghInstance, n string) error {
	t.Helper()
	_, err := testService(t, g.ri).Facts().WriteFact(context.Background(), g.branch, "kb/alpha/x/"+n+".md",
		cmBody(t, "a fact "+n, 0.7), "write "+n, "learn")
	return err
}

// UNUSABLE CONSENSUS ONTOLOGY -> UNWRITABLE, and back (user decision 1).
// Every kind readOntology refuses — missing at every rung, an empty first
// rung, a fatal parse error, a symlink — makes the running repo unwritable
// at the round that brings it: no ontology in force, WritableBranch false,
// an in-process store write refused (no fact written unjudged), status
// reporting the reason. When consensus fixes the file, the next round makes
// it writable again under the NEW ontology, no restart.
//
// SABOTAGE: keep the old ontology on an unusable file (the PR's first
// behaviour) → Ontology not nil, writable → red; drop setIdentity's store
// gate → the store write lands for "missing" → red.
func TestOntologyReload_UnusableMakesTheRepoUnwritableUntilFixed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		brk   func(t *testing.T, o *ghOrigin)
		error string
	}{
		{"missing", func(t *testing.T, o *ghOrigin) { o.remove(t, "drop the ontology", OntologyPath) }, "no ontology at"},
		{"empty", func(t *testing.T, o *ghOrigin) { o.commit(t, "empty it", map[string]string{OntologyPath: ""}) }, "is empty"},
		{"unparseable", func(t *testing.T, o *ghOrigin) {
			o.commit(t, "garble it", map[string]string{OntologyPath: "topics: [::\n"})
		}, "does not parse"},
		{"symlink", func(t *testing.T, o *ghOrigin) { o.symlink(t, OntologyPath) }, "symlink"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, g := newReloadFixture(t)
			// Not for the symlink: a local commit would make the round a
			// real MERGE of main into the agent branch, and the store's merge
			// writes a symlink back as a regular file holding the link text
			// (a separate, pre-existing bug, reported on the PR); a
			// fast-forward keeps the mode.
			if c.name != "symlink" {
				require.NoError(t, writeFactAttempt(t, g, "before"), "a usable ontology: the write lands")
			}

			c.brk(t, o)
			syncRound(t, g)

			require.Nil(t, g.ri.Ontology(), "the old ontology is NOT kept in force")
			require.ErrorContains(t, g.ri.OntologyError(), c.error)
			require.False(t, g.ri.WritableBranch(g.branch), "every authored write is refused")
			require.ErrorIs(t, writeFactAttempt(t, g, "during"), facts.ErrOntologyUnusable,
				"an in-process write is refused too: no fact is written with its validations skipped")
			st, ok := g.ri.OntologyStatus()
			require.True(t, ok)
			require.Contains(t, st.Error, c.error, "status reports the repo unwritable and why")
			tipCommit, _ := readTipBlobAllowingErrors(t, g.ri)
			require.Equal(t, tipCommit, st.Commit, "status names the commit judged")

			o.commit(t, "fix it", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
			syncRound(t, g)

			require.NoError(t, g.ri.OntologyError())
			require.True(t, g.ri.WritableBranch(g.branch), "the fix arrives: writable again, no restart")
			require.NoError(t, placementOK(g.ri, "gamma"), "under the NEW ontology")
			require.NoError(t, writeFactAttempt(t, g, "after"))
			st, _ = g.ri.OntologyStatus()
			require.Empty(t, st.Error)
		})
	}
}

// readTipBlobAllowingErrors is readTipBlob for a tip whose ontology may be a
// symlink (OntologyBlobAt returns the commit with the error).
func readTipBlobAllowingErrors(t *testing.T, ri *RepoInstance) (commit, blob string) {
	t.Helper()
	c, _, b, _ := testService(t, ri).OntologyBlobAt(context.Background(), ri.ReadBranch())
	return c, b
}

// Reverting to the very file that was in force before the break is a fix
// like any other: its blob differs from the unusable one the repo's state
// now names, so it is read and applied.
func TestOntologyReload_RevertingTheBreakRestoresWritability(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	st0, _ := g.ri.OntologyStatus()
	o.commit(t, "break it", map[string]string{OntologyPath: "topics: [::\n"})
	syncRound(t, g)
	require.False(t, g.ri.WritableBranch(g.branch))

	o.commit(t, "revert", map[string]string{OntologyPath: reloadBase})
	syncRound(t, g)
	require.True(t, g.ri.WritableBranch(g.branch))
	st, _ := g.ri.OntologyStatus()
	require.Equal(t, st0.Blob, st.Blob, "the original blob is in force again")
	require.Empty(t, st.Error)
}

// An unusable blob already judged is not read again on later triggers: the
// repo's state names it (src), and the blob comparison skips it. SABOTAGE:
// record an unusable file without its src (identity{ontologyErr: err}) → the
// next round reads it again → red.
func TestOntologyReload_UnusableBlobIsNotReadAgain(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	o.commit(t, "break it", map[string]string{OntologyPath: "topics: [::\n"})
	reads := countReads(g, 0)
	syncRound(t, g)
	require.Error(t, g.ri.OntologyError())
	afterFirst := reads.Load()
	require.GreaterOrEqual(t, afterFirst, int32(1))
	id := g.ri.ident.Load()

	o.commit(t, "unrelated", map[string]string{"notes.txt": "hello\n"})
	syncRound(t, g)
	syncRound(t, g)
	require.Equal(t, afterFirst, reads.Load(), "the judged blob is not read again")
	require.Same(t, id, g.ri.ident.Load(), "nothing is stored again")
}

// THE FLEET ID ON A REPO THAT IS NOT THE FLEET REPOSITORY -> UNAVAILABLE,
// fleet_conflict (user decision 2), as a mount of it is: it leaves the live
// maps and is listed unavailable with CauseFleetConflict. Not applied as a
// fleet repository, not kept writable. Its way back is the mount's (archive
// and purge; a fix on the origin cannot reach a repo that no longer syncs,
// exactly as after a refused boot), so the test pins the state and that a
// fresh mount of the same store refuses it the same way.
//
// SABOTAGE: drop the retireFleetConflict hand-off → the repo stays in
// Manager.Get → red; apply the ontology instead (skip admitFleetOntology) →
// IsFleet true → red.
func TestOntologyReload_FleetIDOnANonFleetRepoIsUnavailableFleetConflict(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	m := g.ri.env.m
	name := g.ri.Name()
	o.commit(t, "become the fleet", map[string]string{OntologyPath: "id: fleet\nname: Fleet\ntopics:\n  alpha:\n    description: alpha\n"})
	syncRound(t, g)

	require.False(t, g.ri.IsFleet(), "never applied as a fleet repository")
	require.False(t, g.ri.WritableBranch(g.branch), "never kept writable (unwritable at once, then unmounted)")
	require.Eventually(t, func() bool { return m.Get(name) == nil }, 10*time.Second, 20*time.Millisecond,
		"the repo leaves the live maps")
	var found *Unavailable
	for _, u := range m.Unavailable() {
		if u.Record.Name == name {
			u := u
			found = &u
		}
	}
	require.NotNil(t, found, "listed unavailable")
	require.Equal(t, CauseFleetConflict, found.Cause)
	require.Equal(t, "unopenable", found.Reason)
	require.Contains(t, found.Detail, "reserved")
	require.Nil(t, m.fleetRepo(), "never the instance's fleet repository")

	// The mount's verdict on the same store is the same.
	_, err := m.mountExisting(name, g.ri.UID(), nil, false)
	require.ErrorIs(t, err, ErrFleetOntologyReserved, "a mount of it is refused the same way")
}

// markFleetConflict puts g's repo in the state a reload leaves it in when the
// fleet id arrives: unwritable with ErrFleetOntologyReserved.
func markFleetConflict(t *testing.T, g *ghInstance) error {
	t.Helper()
	ferr := fmt.Errorf("ontology at %s on %s: %w", OntologyPath, g.branch, ErrFleetOntologyReserved)
	g.ri.ontMu.Lock()
	cur := g.ri.ident.Load()
	g.ri.ident.Store(&identity{ontologyErr: ferr, src: cur.src})
	g.ri.ontMu.Unlock()
	return ferr
}

func unavailableCause(m *Manager, name string) UnavailableCause {
	for _, u := range m.Unavailable() {
		if u.Record.Name == name {
			return u.Cause
		}
	}
	return ""
}

// Manager.Close WAITS for a fleet-conflict retire in flight: it does not
// return, nor close the registry, nor snapshot the machines, until the
// retire has moved the repo to the unavailable list and unmounted it — so
// the repo gets no second Unmount and the retire never reads a closed
// registry. Once Close has begun, no retire starts. SABOTAGE: drop
// retireWg.Wait from Close → Close returns while the retire is held → red;
// drop the retireClosing check → the late start returns true → red.
func TestOntologyReload_CloseWaitsForAFleetConflictRetire(t *testing.T) {
	t.Parallel()
	_, g := newReloadFixture(t)
	m, name := g.ri.env.m, g.ri.Name()
	ferr := markFleetConflict(t, g)
	release := make(chan struct{})
	m.retireHook = func() { <-release }
	require.True(t, m.startFleetConflictRetire(g.ri, ferr))

	closed := make(chan struct{})
	go func() {
		_ = m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a retire was still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close never returned")
	}
	require.Equal(t, CauseFleetConflict, unavailableCause(m, name), "the retire finished before Close's snapshot")
	require.Nil(t, m.Get(name))
	require.False(t, m.startFleetConflictRetire(g.ri, ferr), "no retire starts once Close has begun")
}

// A later reload that applied a FIXED (usable, non-fleet) tip before the
// retire goroutine ran: the retire re-checks under the lock and leaves the
// repo mounted. SABOTAGE: drop the re-check → the fixed repo is unmounted
// → red.
func TestOntologyReload_RetireSkipsARepoFixedMeanwhile(t *testing.T) {
	t.Parallel()
	_, g := newReloadFixture(t)
	m, name := g.ri.env.m, g.ri.Name()
	g.ri.ontMu.Lock()
	fixed := g.ri.ident.Load() // usable, not the fleet id
	g.ri.ontMu.Unlock()
	ferr := markFleetConflict(t, g)
	g.ri.ontMu.Lock()
	g.ri.ident.Store(fixed) // the later reload applied the fix
	g.ri.ontMu.Unlock()

	m.retireFleetConflict(g.ri, ferr)
	require.Same(t, g.ri, m.Get(name), "the fixed repo stays mounted")
	require.Empty(t, unavailableCause(m, name))
	require.True(t, g.ri.WritableBranch(g.branch))
}

// An unchanged blob is not even READ (counted at the file read).
func TestOntologyReload_UnchangedBlobIsNotRead(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	reads := countReads(g, 0)
	o.commit(t, "unrelated", map[string]string{"notes.txt": "hello\n"})
	syncRound(t, g)
	syncRound(t, g)
	require.Equal(t, int32(0), reads.Load())
}

// A reload racing Identify's Enter and Exit (a remount, a swap) never
// resurrects an identity after Exit and never interleaves with Enter's read
// and store; run with -race.
func TestOntologyReload_ConcurrentWithIdentifyAndExit(t *testing.T) {
	t.Parallel()
	o, g := newReloadFixture(t)
	o.commit(t, "add gamma", map[string]string{OntologyPath: reloadBase + "  gamma:\n    description: gamma\n"})
	_, err := testService(t, g.ri).Remote().Sync(context.Background(), g.branch, nil)
	require.NoError(t, err)

	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = g.ri.WithRead(func(svc *store.Service) { g.ri.reloadOntology(ctx, svc) })
				_ = g.ri.Ontology()
				_, _ = g.ri.OntologyStatus()
			}
		}()
	}
	for i := 0; i < 10; i++ {
		identifyStage{}.Exit(g.ri)
		require.NoError(t, identifyStage{}.Enter(ctx, nil, g.ri))
	}
	close(stop)
	wg.Wait()

	// After an Exit, a reload must not establish anything.
	identifyStage{}.Exit(g.ri)
	require.NoError(t, g.ri.WithRead(func(svc *store.Service) { g.ri.reloadOntology(ctx, svc) }))
	require.ErrorIs(t, g.ri.OntologyError(), errNotIdentified, "a reload never resurrects an identity after Exit")
	require.NoError(t, identifyStage{}.Enter(ctx, nil, g.ri))
	require.NoError(t, placementOK(g.ri, "gamma"))
}

// FLEET -> NOT FLEET on the fleet repository (a repo with the fleet door) is
// applied, as the next mount would load it: IsFleet turns false, authored
// writes open, Manager.fleetRepo finds no fleet — and a Warn names the edge.
// Not parallel: it captures the global logger.
func TestOntologyReload_FleetRepoDroppingTheFleetIDIsAppliedAndWarned(t *testing.T) {
	o, g := newReloadFixture(t)
	g.ri.ontMu.Lock()
	g.ri.fleetDoor = true
	g.ri.ontMu.Unlock()
	const fleetish = "id: fleet\nname: Fleet\ntopics:\n  alpha:\n    description: alpha\n"
	o.commit(t, "become the fleet", map[string]string{OntologyPath: fleetish})
	logs := captureLogs(t, zerolog.InfoLevel)
	syncRound(t, g)
	require.True(t, g.ri.IsFleet(), "through the fleet door the fleet id is admitted")
	require.False(t, g.ri.WritableBranch(g.branch), "the fleet repository is read-only for authored writes")
	require.Contains(t, logs.String(), "NOT FLEET -> FLEET")

	o.commit(t, "drop the fleet id", map[string]string{OntologyPath: reloadBase})
	syncRound(t, g)
	require.False(t, g.ri.IsFleet(), "consensus dropped the fleet id: honoured")
	require.True(t, g.ri.WritableBranch(g.branch), "authored writes are open on the former fleet repository")
	require.Nil(t, g.ri.env.m.fleetRepo(), "the instance no longer has a fleet repository")
	require.Contains(t, logs.String(), "FLEET -> NOT FLEET: the fleet repository's consensus ontology dropped the fleet id")
	require.Contains(t, logs.String(), `"level":"warn"`)
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
