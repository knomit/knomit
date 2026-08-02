package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"

	"knomit/internal/backup/proto"
)

// These two tests pin the agent's opMu/mu discipline (see the doc comment on
// Agent). They live here because this is where the discipline is: the CLIENT
// answers status out of one protocol round trip, so nothing there can wedge a
// status the way a stalled remote LIST wedges one here.
//
// Both plant a database whose replica client blocks on demand. That is not a
// convenience — a real stall needs a real network, which is neither
// deterministic nor available in a unit test, and the property being pinned is
// precisely "what happens while a call is stuck".

// blockingReplicaClient blocks inside LTXFiles until released. LTXFiles is the
// call SyncStatus drives via Replica.calcPos, so this stalls a Status.
type blockingReplicaClient struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingReplicaClient) Type() string { return "blocking" }

func (c *blockingReplicaClient) Init(context.Context) error { return nil }

func (c *blockingReplicaClient) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return ltx.NewFileInfoSliceIterator(nil), nil
}

func (c *blockingReplicaClient) OpenLTXFile(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
	return nil, fmt.Errorf("blockingReplicaClient: OpenLTXFile not implemented")
}

func (c *blockingReplicaClient) WriteLTXFile(context.Context, int, ltx.TXID, ltx.TXID, io.Reader) (*ltx.FileInfo, error) {
	return nil, fmt.Errorf("blockingReplicaClient: WriteLTXFile not implemented")
}

func (c *blockingReplicaClient) DeleteLTXFiles(context.Context, []*ltx.FileInfo) error { return nil }

func (c *blockingReplicaClient) DeleteAll(context.Context) error { return nil }

func (c *blockingReplicaClient) SetLogger(*slog.Logger) {}

// TestStatusDoesNotHoldLockAcrossNetworkCall guards the reason Status snapshots
// the tracked set and releases mu before calling SyncStatus.
//
// SyncStatus performs a REMOTE round trip per database — it drains the whole
// level-0 LTX listing to find the remote position, unbounded by bucket size and
// with no caching. Holding mu across that would stall every concurrent Track
// and Untrack for as long as any one of those round trips is in flight, which
// on a slow or stalled object store is indefinitely.
//
// A tracked database is planted whose replica client wedges in LTXFiles, Status
// is started, and Track must then complete promptly. Against a version that
// holds mu for Status's whole body, Track blocks on mu.Lock() until the wedge
// is released — which this test only allows after Track's deadline, so the
// failure is deterministic rather than a matter of timing luck.
func TestStatusDoesNotHoldLockAcrossNetworkCall(t *testing.T) {
	a, home := newTestAgent(t)

	started := make(chan struct{})
	release := make(chan struct{})

	slowDB := litestream.NewDB(filepath.Join(home, "slow.db"))
	slowDB.Replica = litestream.NewReplicaWithClient(slowDB, &blockingReplicaClient{started: started, release: release})

	a.mu.Lock()
	a.dbs["slow"] = tracked{db: slowDB}
	a.mu.Unlock()

	statusDone := make(chan struct{})
	go func() {
		_, _ = a.Status(context.Background())
		close(statusDone)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Status never reached the blocking replica client's LTXFiles call")
	}

	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath, "hello")

	trackDone := make(chan error, 1)
	go func() {
		trackDone <- a.Track(context.Background(), proto.TrackParams{
			Name: "core", Path: dbPath, Rel: "repos/core.db",
		})
	}()

	select {
	case err := <-trackDone:
		close(release)
		<-statusDone
		if err != nil {
			t.Fatalf("Track: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-statusDone
		t.Fatal("Track blocked while Status's remote call was in flight: Status is holding the agent lock across a network call")
	}
}

// blockingUploadClient wedges inside WriteLTXFile and IGNORES context
// cancellation. Ignoring ctx is the whole point: DB.Close cancels the database
// context before waiting on its monitor, so a client that honoured it would
// unwedge itself instantly and prove nothing. This models an object store that
// has simply stopped answering — the case where a close takes tens of seconds.
type blockingUploadClient struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingUploadClient) Type() string                    { return "blocking-upload" }
func (c *blockingUploadClient) Init(context.Context) error      { return nil }
func (c *blockingUploadClient) SetLogger(*slog.Logger)          {}
func (c *blockingUploadClient) DeleteAll(context.Context) error { return nil }

func (c *blockingUploadClient) DeleteLTXFiles(context.Context, []*ltx.FileInfo) error { return nil }

func (c *blockingUploadClient) LTXFiles(context.Context, int, ltx.TXID, bool) (ltx.FileIterator, error) {
	return ltx.NewFileInfoSliceIterator(nil), nil
}

func (c *blockingUploadClient) OpenLTXFile(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
	return nil, fmt.Errorf("blockingUploadClient: OpenLTXFile not implemented")
}

func (c *blockingUploadClient) WriteLTXFile(_ context.Context, _ int, _, _ ltx.TXID, r io.Reader) (*ltx.FileInfo, error) {
	_, _ = io.Copy(io.Discard, r)
	c.once.Do(func() { close(c.started) })
	<-c.release
	return nil, fmt.Errorf("blockingUploadClient: replica is not answering")
}

// TestUntrackDoesNotHoldLockAcrossClose guards the same property for Untrack
// that the test above guards for Status — and it matters more, because
// UnregisterDB CLOSES the database and a close performs a final replica sync
// WITH RETRY (up to litestream's ShutdownSyncTimeout, 30s by default). Holding
// the agent lock across that would freeze every status request — the ops
// surface — for the duration of an object-store hiccup.
//
// A tracked database is wedged mid-upload, Untrack is started against it, and
// Status must still answer promptly. Against a version that holds mu for
// Untrack's whole body, Status blocks on RLock until the wedge is released,
// which this test only allows after Status's deadline.
func TestUntrackDoesNotHoldLockAcrossClose(t *testing.T) {
	a, home := newTestAgent(t)

	// One attempt, no retry loop, so releasing the wedge ends the close quickly.
	a.store.SetShutdownSyncTimeout(0)

	started := make(chan struct{})
	release := make(chan struct{})

	dbPath := filepath.Join(home, "wedged.db")
	makeDB(t, dbPath, "hello")
	wedged := litestream.NewDB(dbPath)
	wedged.MonitorInterval = 50 * time.Millisecond
	wedged.Replica = litestream.NewReplicaWithClient(wedged, &blockingUploadClient{started: started, release: release})
	if err := a.store.RegisterDB(wedged); err != nil {
		t.Fatalf("RegisterDB: %v", err)
	}
	a.mu.Lock()
	a.dbs["wedged"] = tracked{db: wedged}
	a.mu.Unlock()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the planted database never reached its blocking upload")
	}

	untrackDone := make(chan error, 1)
	go func() { untrackDone <- a.Untrack("wedged") }()

	// Give Untrack time to be inside the close before probing.
	time.Sleep(200 * time.Millisecond)

	statusDone := make(chan struct{})
	go func() { _, _ = a.Status(context.Background()); close(statusDone) }()

	select {
	case <-statusDone:
	case <-time.After(2 * time.Second):
		close(release)
		<-untrackDone
		t.Fatal("Status blocked while Untrack was closing a database: Untrack is holding the agent lock across a blocking litestream call")
	}

	close(release)
	if err := <-untrackDone; err != nil {
		t.Logf("Untrack returned %v (expected: the wedged replica refuses its final sync)", err)
	}
}
