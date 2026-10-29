package remotesync

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/testsupport/gitfixture"
)

// countingReader records how many bytes of the pack the fetch consumed.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// newTestProbeStorage is ProbeBranchHistory's overlay over an empty store.
func newTestProbeStorage(t *testing.T, budget int64) *probeStorage {
	t.Helper()
	local, err := storegit.NewMemoryStorer()
	if err != nil {
		t.Fatal(err)
	}
	return &probeStorage{Storage: memory.NewStorage(), local: local, budget: budget}
}

// A pack whose RAW size exceeds the probe budget is refused while it is being
// read, before any of it is parsed or inflated. The pack is all deltas, the
// shape whose bytes go-git's streaming parser buffers uncounted until the
// whole pack has been read; packfile.UpdateObjectStorage is the call go-git's
// fetch makes with the overlay (remote.go fetchPack).
//
// Where it stopped: at most the budget plus one io.Copy chunk (32 KiB) of the
// 4x-budget pack was read, and NO object reached SetEncodedObject (spent 0).
//
// SABOTAGE: delete probeStorage.PackfileWriter -> go-git parses the stream,
// reads the whole pack and only then inflates -> the read assertion is red.
func TestProbeStorage_RawPackOverBudgetStopsBeforeItIsRead(t *testing.T) {
	const budget = 256 << 10
	pack := gitfixture.DeltaPack(t, 16, 64<<10) // ~1 MiB raw, 4x the budget
	if len(pack) < 4*budget {
		t.Fatalf("precondition: the pack (%d bytes) must be well over the budget", len(pack))
	}
	overlay := newTestProbeStorage(t, budget)
	src := &countingReader{r: bytes.NewReader(pack)}

	err := packfile.UpdateObjectStorage(overlay, src)
	if !errors.Is(err, ErrHistoryProbeTooLarge) {
		t.Fatalf("an over-budget pack must be refused with ErrHistoryProbeTooLarge; got %v", err)
	}
	if src.read > budget+32<<10 {
		t.Fatalf("the fetch read %d bytes of a %d-byte pack; it must stop at the %d-byte budget", src.read, len(pack), budget)
	}
	if overlay.spent != 0 {
		t.Fatalf("%d inflated bytes were stored; nothing may be inflated before the raw pack fits", overlay.spent)
	}
	if n := len(overlay.Storage.ObjectStorage.Objects); n != 0 {
		t.Fatalf("%d objects were stored from a refused pack", n)
	}
}

// The non-vacuous half: the same overlay admits the same pack when the budget
// covers it, and every object (the base and each resolved delta) is stored and
// counted as inflated bytes, so the second bound still sees them.
func TestProbeStorage_PackWithinBudgetIsParsed(t *testing.T) {
	const n = 16
	pack := gitfixture.DeltaPack(t, n, 64<<10)
	overlay := newTestProbeStorage(t, int64(4*len(pack)))

	if err := packfile.UpdateObjectStorage(overlay, bytes.NewReader(pack)); err != nil {
		t.Fatalf("a pack within budget must parse: %v", err)
	}
	if got := len(overlay.Storage.ObjectStorage.Objects); got != n+1 {
		t.Fatalf("stored %d objects, want %d", got, n+1)
	}
	if overlay.spent < int64(n*(64<<10)) {
		t.Fatalf("inflated bytes counted %d, want at least %d", overlay.spent, n*(64<<10))
	}
}
