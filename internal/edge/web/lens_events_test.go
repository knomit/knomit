package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/api/kb"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/foundation/auth"
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

// A member PINNED to a branch other than its read branch is covered: the
// store syncs the committed branch on every commit (NotifyCommit), the repo's
// status event fires, and the pinned branch's settled watermark moves. (The
// review's probe: a notice within ~1.4s for a member pinned to agent/side.)
//
// Sabotage: watch each member at its read branch instead of the pinned one,
// and the agent/side write produces no notice.
func TestLensEvents_PinnedBranchMemberIsCovered(t *testing.T) {
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
	beta := m.Get("beta")
	ctx := context.Background()
	require.NoError(t, beta.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(ctx, "agent/side", beta.ReadBranch()))
	}))
	r := (&Server{Manager: m, Embedder: emb}).NewAPIRouter()
	createLens(t, m, r, `{"name":"eng","write":"alpha","reads":[{"repo":"beta","branch":"agent/side"}]}`)
	s := openLensEvents(t, r, "eng")
	s.none(t, 200*time.Millisecond)

	require.NoError(t, beta.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(ctx, "agent/side", "kb/b/pinned.md", factText(t, "kb/b/pinned.md", "pinned", 0.9, nil), "pinned", "learn")
		require.NoError(t, err)
	}))
	f := s.next(t, 10*time.Second)
	var ev struct{ Repo, Branch, Head string }
	require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
	require.Equal(t, "beta", ev.Repo)
	require.Equal(t, "agent/side", ev.Branch)
	require.NoError(t, beta.WithRead(func(svc *store.Service) {
		wm, _, settled, err := svc.HistoryQuery().SettledWatermark(ctx, "agent/side")
		require.NoError(t, err)
		require.True(t, settled)
		require.Equal(t, wm, ev.Head)
	}))
}

// L1 (#749 review): the middleware reads the lens BEFORE the handler takes
// LensRegistry.Changed, so an edit in between closes no channel the stream
// holds. The handler reads the lens again after taking it, and a membership
// that differs from the binding ends the stream at once. Driven with a binding
// resolved before the edit, as the race would leave it.
//
// Sabotage: skip the re-read (lensReadsMatch) and the stale stream stays open.
func TestLensEvents_StaleBindingEndsTheStream(t *testing.T) {
	env := searchLens(t, "alpha", "beta", "gamma")
	staleCtx, err := repos.ResolveLensBinding(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindHost, ID: "t"}), env.m, "eng")
	require.NoError(t, err)
	rec := patchLens(t, env.m, env.r, "eng", `{"reads":[{"repo":"beta"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	br, lens := chi.NewRouter(), chi.NewRouter()
	lens.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(repos.WithBinding(r.Context(), repos.BindingFromContext(staleCtx)), repos.BindingFromContext(staleCtx).Write())))
		})
	})
	kb.Mount(chi.NewRouter(), br, lens, "kb", env.emb, env.m)
	srv := httptest.NewServer(lens)
	t.Cleanup(srv.Close)
	reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "the stale stream must end on its own, not hang until the deadline")
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() { _, err := bufio.NewReader(resp.Body).ReadString(0); done <- err }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stream opened on a stale binding did not end")
	}
}

// failSettled makes ri's SettledWatermark reads fail while on is set: what a
// Rebuild holding the branch unsettled looks like to the stream ("" head).
func failSettled(t *testing.T, ri *repos.RepoInstance, on *atomicBool) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		restore := svc.Search().SetOverridesForTest(index.Overrides{Reads: func(method string) error {
			if method == "SettledWatermark" && on.get() {
				return errors.New("held by a rebuild")
			}
			return nil
		}})
		t.Cleanup(restore)
	}))
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// L3 (#749 review): a member whose head cannot be read settled when its
// status event arrives (a Rebuild holds the branch) stays DIRTY: its notice
// goes out on the next wake, here the write repo's own status event.
//
// Sabotage: clear the dirty mark when the head is "" and beta's notice is
// lost: only alpha's arrives.
func TestLensEvents_UnsettledMemberStaysDirty(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	s := openLensEvents(t, env.r, "eng")
	s.none(t, 200*time.Millisecond)
	var held atomicBool
	failSettled(t, env.m.Get("beta"), &held)
	held.set(true)
	writeFactsInOneCommit(t, env.m.Get("beta"), factSpec{path: "kb/b/held.md", title: "held"})
	s.none(t, quietWindow) // the status event came and went: nothing readable yet
	held.set(false)
	writeFactsInOneCommit(t, env.m.Get("alpha"), factSpec{path: "kb/a/wake.md", title: "wake"})
	got := map[string]bool{}
	for len(got) < 2 {
		f := s.next(t, 10*time.Second)
		var ev struct{ Repo string }
		require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
		got[ev.Repo] = true
	}
	require.True(t, got["beta"], "the held member's notice was kept, not lost")
}

// L3 (#749 review): an index job finishing (IndexEvent "ready") is a wake too,
// so a held member's notice goes out when its Rebuild ends even if nothing
// else ever commits.
//
// Sabotage: ignore IndexEvent and the notice never comes.
func TestLensEvents_IndexReadyWakesAHeldMember(t *testing.T) {
	env := searchLens(t, "alpha", "beta")
	s := openLensEvents(t, env.r, "eng")
	s.none(t, 200*time.Millisecond)
	beta := env.m.Get("beta")
	var held atomicBool
	failSettled(t, beta, &held)
	held.set(true)
	writeFactsInOneCommit(t, beta, factSpec{path: "kb/b/held.md", title: "held"})
	s.none(t, quietWindow)
	held.set(false)
	rec := httptest.NewRecorder()
	env.r.ServeHTTP(rec, asOwner(httptest.NewRequest(http.MethodPost,
		"/repos/beta/branches/"+strings.ReplaceAll(beta.ReadBranch(), "/", ":")+"/index-rebuilds", nil)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	f := s.next(t, 20*time.Second)
	var ev struct{ Repo string }
	require.NoError(t, json.Unmarshal([]byte(f.data), &ev))
	require.Equal(t, "beta", ev.Repo)
}
