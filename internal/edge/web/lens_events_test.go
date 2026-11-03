package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
)

// lensEventStream is an open GET /lenses/{lens}/events, read frame by frame.
type lensEventStream struct {
	mu     sync.Mutex
	kinds  []string // every `event:` name seen, in order
	frames chan lensFrame
	ended  chan struct{}
}

type lensFrame struct {
	kind string
	data string
}

// openLensEvents opens the lens stream on a real server (the whole middleware
// chain) and reads it in the background until the test or the server ends it.
func openLensEvents(t *testing.T, h http.Handler, lens string) *lensEventStream {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/lenses/"+lens+"/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	s := &lensEventStream{frames: make(chan lensFrame, 64), ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		kind := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
				s.mu.Lock()
				s.kinds = append(s.kinds, kind)
				s.mu.Unlock()
			case strings.HasPrefix(line, "data: "):
				s.frames <- lensFrame{kind: kind, data: strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return s
}

func (s *lensEventStream) seenKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.kinds...)
}

// next waits for the next data frame, or fails after d.
func (s *lensEventStream) next(t *testing.T, d time.Duration) lensFrame {
	t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-s.ended:
		t.Fatal("the stream ended")
	case <-time.After(d):
		t.Fatalf("no event within %s", d)
	}
	return lensFrame{}
}

// none asserts no data frame arrives within d (longer than the commit
// observer's debounce, so an event that was coming has had time to).
func (s *lensEventStream) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case f := <-s.frames:
		t.Fatalf("unexpected event %s %s", f.kind, f.data)
	case <-time.After(d):
	}
}

// settledHead is the member branch's settled index watermark.
func settledHead(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	var wm string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		w, _, settled, err := svc.HistoryQuery().SettledWatermark(context.Background(), ri.ReadBranch())
		require.NoError(t, err)
		require.True(t, settled, "the member's index is settled when its notice arrives")
		wm = w
	}))
	return wm
}

const quietWindow = 2500 * time.Millisecond // > the commit observer's 1s debounce plus its sync

// A write to a READ member (not the write repo) produces ONE `content` notice
// for that member, after its index absorbed the commit: the head it carries
// is the member's settled watermark and the lens list already lists the new
// fact. A write to the write repo still produces one for the write repo. No
// other event kind ever appears on the stream.
//
// Sabotage: watch only the write repo (skip the read members) and the beta
// write produces no notice; forward the hub's status events as `status`
// frames and the kinds check fails.
func TestLensEvents_ReadMemberWriteNotifiesAfterIndexing(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	s := openLensEvents(t, env.r, "eng")
	s.none(t, 300*time.Millisecond) // nothing at connect

	writeFactsInOneCommit(t, env.m.Get("beta"), factSpec{path: "kb/b/new.md", title: "beta new"})
	f := s.next(t, 10*time.Second)
	require.Equal(t, "content", f.kind)
	var ev struct{ Repo, ID, Branch, Head string }
	require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
	beta := env.m.Get("beta")
	require.Equal(t, "beta", ev.Repo)
	require.Len(t, ev.ID, 12)
	require.Equal(t, beta.ReadBranch(), ev.Branch)
	require.Equal(t, settledHead(t, beta), ev.Head, "the head is the member's settled index watermark")
	body := decodeLensFacts(t, env.get(t, "/lenses/eng/facts?limit=50"))
	var paths []string
	for _, fa := range body.Facts {
		paths = append(paths, fa.Path)
	}
	require.Contains(t, paths, "kb://"+ev.ID+"/kb/b/new.md", "a read issued on the notice sees the change")
	s.none(t, quietWindow) // one notice, not one per event

	writeFactsInOneCommit(t, env.m.Get("alpha"), factSpec{path: "kb/a/new.md", title: "alpha new"})
	f = s.next(t, 10*time.Second)
	require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
	require.Equal(t, "alpha", ev.Repo, "the write repo is covered too")

	for _, k := range s.seenKinds() {
		require.Equal(t, "content", k, "the stream carries content notices only")
	}
}

// A burst of commits to one member gives ONE notice carrying the latest head.
// A commit that does not change the member's content at the branch the lens
// reads (here: a commit on another branch of the same repo, which still fires
// the repo's status event) gives NONE.
//
// Sabotage: drop the "head moved" check and the other-branch commit produces
// a notice with an unchanged head.
func TestLensEvents_BurstCoalescesAndUnchangedContentIsSilent(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	s := openLensEvents(t, env.r, "eng")
	beta := env.m.Get("beta")

	for i := 0; i < 8; i++ {
		writeCosFact(t, beta, fmt.Sprintf("kb/b/burst%d.md", i), fmt.Sprintf("burst %d", i), 0.9, nil)
	}
	f := s.next(t, 10*time.Second)
	var ev struct{ Repo, Head string }
	require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
	require.Equal(t, "beta", ev.Repo)
	require.Equal(t, settledHead(t, beta), ev.Head, "the notice carries the burst's latest head")
	s.none(t, quietWindow)

	// A commit on another branch of beta: the repo's status event fires, its
	// read branch's content does not change.
	ctx := context.Background()
	require.NoError(t, beta.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(ctx, "agent/side", beta.ReadBranch()))
		_, err := svc.Facts().WriteFact(ctx, "agent/side", "kb/b/side.md", factText(t, "kb/b/side.md", "side", 0.9, nil), "side", "learn")
		require.NoError(t, err)
	}))
	s.none(t, quietWindow)
}

// MEMBERSHIP CHANGE: the stream serves the membership resolved at connect and
// ENDS when the lens is edited, so the client reconnects onto the new one.
//
// Sabotage: ignore LensRegistry.Changed and the stream stays open.
func TestLensEvents_LensEditEndsTheStream(t *testing.T) {
	env := searchLens(t, "alpha", "beta", "gamma")
	s := openLensEvents(t, env.r, "eng")
	s.none(t, 200*time.Millisecond)
	rec := patchLens(t, env.m, env.r, "eng", `{"reads":[{"repo":"beta"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	select {
	case <-s.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end after the lens was edited")
	}
}

// The route takes no query parameter (the #720 rule).
func TestLensEvents_UnknownParamIs400(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	rec := httptest.NewRecorder()
	env.r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/eng/events?since=1", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `unknown parameter \"since\"`)
}
