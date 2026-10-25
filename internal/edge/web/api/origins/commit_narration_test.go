package origins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"knomit/internal/corpus/repos"
)

// gatedWriter is a response writer whose first "swapping" event stalls the
// stream reader (the handler's goroutine) until proceed returns, so the
// machine can finish while the reader sits between two Watch reads.
type gatedWriter struct {
	*streamRecorder
	swapping chan struct{}
	proceed  func()
	once     sync.Once
}

func (g *gatedWriter) Write(b []byte) (int, error) {
	n, err := g.streamRecorder.Write(b)
	if strings.Contains(string(b), `"phase":"swapping"`) {
		g.once.Do(func() {
			close(g.swapping)
			g.proceed()
		})
	}
	return n, err
}

// TestHandleCommit_Disjoint_NarratesConfiguringWhenTheReaderIsLate pins the
// stream's order (swapping, configuring, done) deterministically, without
// depending on scheduling. Machine.Watch is lossy (cap 1, latest wins): the
// machine is held in populate until the reader has written "swapping", then
// the reader is stalled in that very write until the machine is ready. Every
// open/identify transition is gone from the watch by then, and the reader
// meets done with phase "swapping" and nothing pending that names configuring.
func TestHandleCommit_Disjoint_NarratesConfiguringWhenTheReaderIsLate(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	var armed atomic.Bool
	swapping := make(chan struct{})
	hook := func(st repos.StageID, point string, ctx context.Context) {
		if !armed.Load() || st != repos.StagePopulate || point != "enter" {
			return
		}
		select {
		case <-swapping:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	f := newCommitFixture(t, keyPath, hook)

	rec := &gatedWriter{streamRecorder: newStreamRecorder(), swapping: swapping}
	rec.proceed = func() {
		// The close above releases the hook; hold this write (the reader)
		// until the machine has finished its walk.
		deadline := time.Now().Add(10 * time.Second)
		for f.ri.Status().Stage != "ready" && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	armed.Store(true)
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions/"+f.sess.ID+"/commit", nil)
	f.s.ServeHTTP(rec, req)

	body := rec.body()
	iSwap := strings.Index(body, `"phase":"swapping"`)
	iConf := strings.Index(body, `"phase":"configuring"`)
	iDone := strings.Index(body, `"phase":"done"`)
	if iSwap < 0 || iConf < 0 || iDone < 0 || !(iSwap < iConf && iConf < iDone) {
		t.Errorf("want swapping, configuring, done in that order; body=%s", body)
	}
}
