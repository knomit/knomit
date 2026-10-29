package remotesync

import (
	"bytes"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// NewBudgetedPackWriter is the storer.PackfileWriter body of a budgeted
// memory probe store (ProbeBranchHistory's overlay, the create probe's
// budgetedStorage). go-git hands a fetched pack to a storer that implements
// PackfileWriter as a byte stream (packfile.UpdateObjectStorage), so the RAW
// pack is counted here, as it arrives: past budget bytes (<= 0: unbounded)
// Write fails with tooLarge, which aborts the fetch, and nothing is parsed or
// inflated.
//
// Why not let go-git parse the stream: without a PackfileWriter it parses the
// non-seekable stream itself, and go-git v5.18.0's parser then buffers every
// delta's raw bytes until the whole pack is read, so a budget counted in
// SetEncodedObject (inflated objects) sees no delta until after the transfer.
//
// Close parses the buffered pack with a SEEKABLE scanner, which keeps no copy
// of the deltas, into target. target receives every object through its own
// SetEncodedObject, so a store that also counts inflated bytes there keeps
// that second bound, and a thin pack resolves its external bases through
// target's reads.
//
// What is bounded: the raw pack by the budget, and the objects target admits
// by the budget. What is not: go-git inflates ONE object fully before
// SetEncodedObject counts it, so a highly compressible object can be held
// transiently beyond the budget (its compressed size times the zlib ratio:
// a 4 MiB blob of zeros packs to about 8 KiB) before it is refused. Nothing
// is admitted unchecked; the bound on memory is not "twice the budget".
func NewBudgetedPackWriter(target storer.EncodedObjectStorer, budget int64, tooLarge error) io.WriteCloser {
	return &budgetedPackWriter{target: target, budget: budget, tooLarge: tooLarge}
}

type budgetedPackWriter struct {
	target   storer.EncodedObjectStorer
	budget   int64
	tooLarge error
	buf      bytes.Buffer
	over     bool
}

func (w *budgetedPackWriter) Write(p []byte) (int, error) {
	if w.over {
		return 0, w.tooLarge
	}
	if w.budget > 0 && int64(w.buf.Len())+int64(len(p)) > w.budget {
		w.over = true
		return 0, w.tooLarge
	}
	return w.buf.Write(p)
}

// Close parses what was written, unless the budget was exceeded: that Write
// already returned the error the fetch reports.
func (w *budgetedPackWriter) Close() error {
	if w.over {
		return nil
	}
	defer w.buf.Reset()
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(bytes.NewReader(w.buf.Bytes())), w.target)
	if err != nil {
		return fmt.Errorf("probe: parse the fetched pack: %w", err)
	}
	if _, err := parser.Parse(); err != nil {
		return err
	}
	return nil
}
