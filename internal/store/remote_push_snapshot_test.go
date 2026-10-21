package store

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/store/remotesync"
)

// F21 S1: the snapshot ("bookmark") push. Push reads the branch tip under the
// write lock, releases it, and pushes that hash. These tests park a push at
// storeHooks.afterSnapshot — after the snapshot, before any network — against
// a real local bare origin.

const snapBranch = "agent/test"

// storeHooks mirrors the child's two test seams; setStoreHooks installs them
// through the child's test door and removes them at cleanup.
type storeHooks struct {
	afterSnapshot func(branch string, snap plumbing.Hash)
	afterFetch    func() error
}

func setStoreHooks(t *testing.T, h storeHooks) {
	t.Helper()
	t.Cleanup(remotesync.SetStoreHooksForTest(h.afterSnapshot, h.afterFetch))
}

// parkFirst parks the FIRST push that reaches afterSnapshot until release;
// later pushes pass through. snaps records every snapshot in order.
type parkFirst struct {
	calls   atomic.Int64
	reached chan plumbing.Hash
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	snaps   []plumbing.Hash
}

func parkFirstPush(t *testing.T) *parkFirst {
	t.Helper()
	p := &parkFirst{reached: make(chan plumbing.Hash, 1), release: make(chan struct{})}
	setStoreHooks(t, storeHooks{afterSnapshot: func(_ string, snap plumbing.Hash) {
		p.mu.Lock()
		p.snaps = append(p.snaps, snap)
		p.mu.Unlock()
		if p.calls.Add(1) == 1 {
			p.reached <- snap
			<-p.release
		}
	}})
	t.Cleanup(p.free)
	return p
}

func (p *parkFirst) free() { p.once.Do(func() { close(p.release) }) }

func (p *parkFirst) waitParked(t *testing.T) plumbing.Hash {
	t.Helper()
	select {
	case s := <-p.reached:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the push never reached its snapshot")
		return plumbing.ZeroHash
	}
}

// newSnapshotPushFixture: a repo on agent/test with one fact, configured
// against a real local bare origin.
func newSnapshotPushFixture(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, snapBranch))
	bare := filepath.Join(dir, "origin.git")
	_, err = gogit.PlainInit(bare, true)
	require.NoError(t, err)
	svc.Remote().SetOrigin(&remotesync.Origin{URL: bare, Branch: "main"})
	require.NoError(t, svc.ConfigureRemote(bare, "main", snapBranch))
	writeTestFact(t, svc, "kb/notes/seed.md", "seed", "seed")
	return svc, bare
}

func localTip(t *testing.T, svc *Service) plumbing.Hash {
	t.Helper()
	r, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName(snapBranch))
	require.NoError(t, err)
	return r.Hash()
}

func trackingTip(t *testing.T, svc *Service) plumbing.Hash {
	t.Helper()
	r, err := svc.rh.Storer().Reference(plumbing.NewRemoteReferenceName("origin", snapBranch))
	require.NoError(t, err)
	return r.Hash()
}

func originTip(t *testing.T, bare string) plumbing.Hash {
	t.Helper()
	repo, err := gogit.PlainOpen(bare)
	require.NoError(t, err)
	r, err := repo.Reference(plumbing.NewBranchReferenceName(snapBranch), true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return r.Hash()
}

type pushOutcome struct {
	res remotesync.PushResult
	err error
}

func pushAsync(svc *Service) <-chan pushOutcome {
	ch := make(chan pushOutcome, 1)
	go func() {
		res, err := svc.Remote().Push(context.Background(), snapBranch, nil)
		ch <- pushOutcome{res, err}
	}()
	return ch
}

func waitPush(t *testing.T, ch <-chan pushOutcome) pushOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(20 * time.Second):
		t.Fatal("the push never returned")
		return pushOutcome{}
	}
}

// P1 WritesNotBlockedDuringPush: a push parked after its snapshot (standing
// in for a push on a slow network) does not hold the branch's write lock, so
// a fact write on the same branch completes while it is parked. Sabotage:
// hold LockBranch across the push → the write blocks → red.
func TestPush_WritesNotBlockedDuringPush(t *testing.T) {
	svc, _ := newSnapshotPushFixture(t)
	park := parkFirstPush(t)
	done := pushAsync(svc)
	park.waitParked(t)

	wrote := make(chan error, 1)
	go func() {
		_, err := svc.Facts().WriteFact(context.Background(), snapBranch, "kb/notes/during.md",
			"---\ntype: observation\n---\n# during\n\nduring", "write during", "test")
		wrote <- err
	}()
	select {
	case err := <-wrote:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		park.free()
		waitPush(t, done)
		t.Fatal("a write waited on a push that was parked off the write lock")
	}
	park.free()
	require.NoError(t, waitPush(t, done).err)
}

// P2 SendsSnapshotThenRest: the push sends exactly its snapshot S1 even
// though C2 landed before it reached the network; the tracking ref is S1,
// and the next Push sends C2. Sabotage: (a) the refspec by NAME (go-git reads
// the ref late) → the first push lands C2 and "origin = S1" is red; (b) the
// tracking ref set to the re-read local tip after the push → the next Push
// thinks it is up to date and C2 never reaches the origin → red.
func TestPush_SendsSnapshotThenRest(t *testing.T) {
	svc, bare := newSnapshotPushFixture(t)
	park := parkFirstPush(t)
	done := pushAsync(svc)
	s1 := park.waitParked(t)
	require.Equal(t, localTip(t, svc), s1)

	writeTestFact(t, svc, "kb/notes/c2.md", "c2", "c2")
	c2 := localTip(t, svc)
	require.NotEqual(t, s1, c2)

	park.free()
	o := waitPush(t, done)
	require.NoError(t, o.err)
	require.True(t, o.res.Pushed)
	require.Equal(t, s1, originTip(t, bare), "the push sends its snapshot, not the later commit")
	require.Equal(t, s1, trackingTip(t, svc), "the tracking ref is what was pushed")

	o2, err := svc.Remote().Push(context.Background(), snapBranch, nil)
	require.NoError(t, err)
	require.True(t, o2.Pushed, "the commit made during the push goes out on the next push")
	require.Equal(t, c2, originTip(t, bare))
	require.Equal(t, c2, trackingTip(t, svc))
}

// P3 SerializedNoOlderAfterNewer: push A (snapshot T1) is parked; a commit
// makes T2; push B starts and must wait on pushMu (it never reaches its
// snapshot while A is parked); released, A lands T1 and B then lands T2, so
// the final origin is T2. Sabotage: drop pushMu → B lands T2 while A is
// parked, then A force-pushes T1 over it → final origin T1 → red.
func TestPush_SerializedNoOlderAfterNewer(t *testing.T) {
	svc, bare := newSnapshotPushFixture(t)
	park := parkFirstPush(t)
	doneA := pushAsync(svc)
	t1 := park.waitParked(t)

	writeTestFact(t, svc, "kb/notes/t2.md", "t2", "t2")
	t2 := localTip(t, svc)
	doneB := pushAsync(svc)

	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int64(1), park.calls.Load(), "push B took a snapshot while push A was still in flight")

	park.free()
	require.NoError(t, waitPush(t, doneA).err)
	require.NoError(t, waitPush(t, doneB).err)
	require.Equal(t, []plumbing.Hash{t1, t2}, park.snaps, "B's snapshot is taken after A finished")
	require.Equal(t, t2, originTip(t, bare), "an older tip never lands after a newer one")
	require.Equal(t, t2, trackingTip(t, svc))
}

// P4 RewriteDuringOtherCallerPush: a fleet-style push is parked with S_old;
// the branch is then rewritten to S_new (the rebase fallback's shape: S_new
// is NOT a descendant of S_old); released, the origin briefly holds S_old,
// and the loop's next push — waiting on pushMu meanwhile — force-pushes
// S_new. Sabotage: skip the push when the tracking ref is not an ancestor of
// the snapshot → the origin stays S_old → red.
func TestPush_RewriteDuringOtherCallerPush(t *testing.T) {
	svc, bare := newSnapshotPushFixture(t)
	base := localTip(t, svc)
	_, err := svc.Remote().Push(context.Background(), snapBranch, nil)
	require.NoError(t, err)
	require.Equal(t, base, originTip(t, bare))

	writeTestFact(t, svc, "kb/notes/old.md", "old", "old")
	sOld := localTip(t, svc)

	park := parkFirstPush(t)
	doneFleet := pushAsync(svc)
	require.Equal(t, sOld, park.waitParked(t))

	// The rewrite: the branch goes back to base and gains a different commit.
	func() {
		defer svc.rh.LockBranch(snapBranch)()
		require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(snapBranch), base)))
	}()
	writeTestFact(t, svc, "kb/notes/new.md", "new", "new")
	sNew := localTip(t, svc)
	newCommit, err := svc.rh.Repository().CommitObject(sNew)
	require.NoError(t, err)
	oldCommit, err := svc.rh.Repository().CommitObject(sOld)
	require.NoError(t, err)
	isAnc, err := oldCommit.IsAncestor(newCommit)
	require.NoError(t, err)
	require.False(t, isAnc, "fixture: S_new must not descend from S_old")

	doneLoop := pushAsync(svc)
	park.free()
	require.NoError(t, waitPush(t, doneFleet).err)
	o := waitPush(t, doneLoop)
	require.NoError(t, o.err)
	require.True(t, o.res.Pushed)
	require.Equal(t, sNew, originTip(t, bare), "the origin converges on the rewritten tip")
	require.Equal(t, localTip(t, svc), originTip(t, bare))
}

// P5 (review N7) UnreadableSnapshotIsAnError: when the snapshot's commit
// cannot be read, go-git adds no command and answers "already up to date",
// so Push would report Pushed:false with no error while nothing was sent.
// Push must report it as an error, persisted on the push status. Sabotage:
// drop the CommitObject guard → nil error, Pushed:false → red.
func TestPush_UnreadableSnapshotIsAnError(t *testing.T) {
	svc, bare := newSnapshotPushFixture(t)
	_, err := svc.Remote().Push(context.Background(), snapBranch, nil)
	require.NoError(t, err)
	before := originTip(t, bare)

	writeTestFact(t, svc, "kb/notes/lost.md", "lost", "lost")
	lost := localTip(t, svc)
	require.NoError(t, svc.rh.Storer().DeleteObjectForTest(lost))

	res, err := svc.Remote().Push(context.Background(), snapBranch, nil)
	require.Error(t, err, "a push that sent nothing must not read as up to date")
	require.Contains(t, err.Error(), lost.String())
	require.False(t, res.Pushed)
	require.Equal(t, before, originTip(t, bare))
	rem, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastPushStatus)
	require.Equal(t, "error", *rem.LastPushStatus)
}

// P6 HungOriginDoesNotStallWrites: P1's property against a REAL hang — an
// origin that accepts the connection and never answers. While the push waits
// on it, a burst of writes all complete, and the push is still hung when the
// last one returns. The proof is the ORDER (every write finished before the
// push did), not a latency bound: a wall-clock bound on one write measures
// machine load, not the lock (it read 1.138 s against a 1 s bound under
// parallel test runs). The 10 s wait is only a hang detector.
// Sabotage: hold LockBranch across PushContext → every write waits until the
// push gives up → the writes miss the hang detector (or the push returns
// first) → red.
func TestPush_HungOriginDoesNotStallWrites(t *testing.T) {
	svc, _ := newSnapshotPushFixture(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var conns sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); conns.Wait() })
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func() { defer conns.Done(); _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	hung := "http://" + ln.Addr().String() + "/kb.git"
	svc.Remote().SetOrigin(&remotesync.Origin{URL: hung, Branch: "main"})
	require.NoError(t, svc.ConfigureRemote(hung, "main", snapBranch))

	// The watchdog stands in for network_timeout: it ends the push whatever
	// happens. It is well past the hang detector below, so a healthy run never
	// sees the push end before the writes do, however loaded the machine.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.Remote().Push(ctx, snapBranch, nil); done <- err }()
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the push never reached the origin")
	}

	const writes = 10
	written := make(chan error, 1)
	go func() {
		for i := 0; i < writes; i++ {
			if _, err := svc.Facts().WriteFact(context.Background(), snapBranch, fmt.Sprintf("kb/notes/hung-%d.md", i),
				"---\ntype: observation\n---\n# hung\n\nhung", "write during a hung push", "test"); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	select {
	case err := <-written:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("writes waited on the hung push")
	}
	select {
	case err := <-done:
		t.Fatalf("the push returned while the origin hung (err=%v); the probe proves nothing", err)
	default:
	}
	cancel()
	<-done
}
