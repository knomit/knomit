package repos

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/packfile"

	"knomit/internal/testsupport/gitfixture"
)

type countingPackReader struct {
	r    io.Reader
	read int
}

func (c *countingPackReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// The create probe's budgetedStorage shares the history probe's raw-pack
// bound (remotesync.NewBudgetedPackWriter): an all-delta pack 4x the budget
// is refused after at most the budget plus one io.Copy chunk is read, with
// nothing inflated; the same pack within budget parses whole.
//
// SABOTAGE: delete budgetedStorage.PackfileWriter -> go-git's streaming
// parser reads the whole pack before inflating a delta -> red.
func TestBudgetedStorage_RawPackOverBudgetStopsBeforeItIsRead(t *testing.T) {
	const budget = 256 << 10
	pack := gitfixture.DeltaPack(t, 16, 64<<10)
	if len(pack) < 4*budget {
		t.Fatalf("precondition: the pack (%d bytes) must be well over the budget", len(pack))
	}
	st := newBudgetedStorage(budget)
	src := &countingPackReader{r: bytes.NewReader(pack)}
	err := packfile.UpdateObjectStorage(st, src)
	if !errors.Is(err, errProbeTooLarge) {
		t.Fatalf("an over-budget pack must be refused with errProbeTooLarge; got %v", err)
	}
	if src.read > budget+32<<10 {
		t.Fatalf("read %d bytes of a %d-byte pack; must stop at the %d-byte budget", src.read, len(pack), budget)
	}
	if st.spent != 0 || len(st.Storage.ObjectStorage.Objects) != 0 {
		t.Fatalf("nothing may be inflated or stored from a refused pack: spent %d, %d objects", st.spent, len(st.Storage.ObjectStorage.Objects))
	}

	ok := newBudgetedStorage(int64(4 * len(pack)))
	if err := packfile.UpdateObjectStorage(ok, bytes.NewReader(pack)); err != nil {
		t.Fatalf("the same pack within budget must parse: %v", err)
	}
	if got := len(ok.Storage.ObjectStorage.Objects); got != 17 {
		t.Fatalf("stored %d objects, want 17", got)
	}
}
