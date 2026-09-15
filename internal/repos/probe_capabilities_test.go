package repos

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

func mustDefaultOntology(t *testing.T) string {
	t.Helper()
	b, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	return string(b)
}

// recordUploadPack wraps h, accumulating the bodies of the upload-pack
// requests so a test can see whether the probe asked for a depth.
func recordUploadPack(h http.Handler) (http.Handler, *atomic.Value) {
	var last atomic.Value
	last.Store("")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			b, _ := io.ReadAll(r.Body)
			last.Store(last.Load().(string) + string(b))
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.ServeHTTP(w, r)
	}), &last
}

// knomitServed serves a knomit store through its own handler, which advertises
// shallow.
func knomitServed(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{OntologyPath: mustDefaultOntology(t)}, "main"))
	h, last := recordUploadPack(svc.Handler())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, last
}

type staticLoader struct{ sto storer.Storer }

func (l staticLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.sto, nil }

// nonShallowServed serves a repo through go-git's BUILT-IN upload-pack server,
// which advertises only agent and ofs-delta. That is the shape knomit itself
// served before this change, so this fixture stands in for an older knomit and
// for any server that cannot do shallow.
//
// file:// is NOT that fixture, whatever the plan assumed: go-git's file
// transport execs the real git-upload-pack binary, which advertises shallow
// like any modern git.
func nonShallowServed(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	sto := memory.NewStorage()
	repo, err := gogit.Init(sto, memfs.New())
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	f, err := wt.Filesystem.Create(OntologyPath)
	require.NoError(t, err)
	_, err = f.Write([]byte(mustDefaultOntology(t)))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = wt.Add(OntologyPath)
	require.NoError(t, err)
	sig := &object.Signature{Name: "test", Email: "test@local", When: time.Now()}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	srvr := gogitserver.NewServer(staticLoader{sto})
	mux := http.NewServeMux()
	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		sess, serr := srvr.NewUploadPackSession(&transport.Endpoint{}, nil)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()
		ar, aerr := sess.AdvertisedReferencesContext(r.Context())
		if aerr != nil {
			http.Error(w, aerr.Error(), http.StatusInternalServerError)
			return
		}
		ar.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_ = ar.Encode(w)
	})
	mux.HandleFunc("/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			http.Error(w, rerr.Error(), http.StatusBadRequest)
			return
		}
		req := packp.NewUploadPackRequest()
		if derr := req.Decode(bytes.NewReader(body)); derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		sess, serr := srvr.NewUploadPackSession(&transport.Endpoint{}, nil)
		if serr != nil {
			http.Error(w, serr.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()
		if _, aerr := sess.AdvertisedReferencesContext(r.Context()); aerr != nil {
			http.Error(w, aerr.Error(), http.StatusInternalServerError)
			return
		}
		resp, uerr := sess.UploadPack(r.Context(), req)
		if uerr != nil {
			http.Error(w, uerr.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Close()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = resp.Encode(w)
	})
	wrapped, last := recordUploadPack(mux)
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv, last
}

// requireNoShallowAdvertised is what makes nonShallowServed a fixture rather
// than an assumption.
func requireNoShallowAdvertised(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url + "/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), "shallow", "fixture must not advertise shallow")
}

// The probe asks for one commit when the server says it can serve one.
func TestProbeInitialized_UsesDepth1WhenShallowAdvertised(t *testing.T) {
	srv, sent := knomitServed(t)
	m := newTestManager(t)
	require.NoError(t, m.Start())

	res, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: srv.URL})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized, res.Detail)
	require.Contains(t, sent.Load().(string), "deepen 1",
		"a server advertising shallow must be asked for a shallow clone")
}

// A server that does NOT advertise shallow must still be probed — with a full
// single-branch clone. Asking it for a depth is what produced the HTTP 500
// this whole change started from.
func TestProbeInitialized_FullCloneWhenShallowNotAdvertised(t *testing.T) {
	srv, sent := nonShallowServed(t)
	requireNoShallowAdvertised(t, srv.URL)

	m := newTestManager(t)
	require.NoError(t, m.Start())
	res, err := m.ProbeInitialized(context.Background(), OriginSpec{URL: srv.URL})
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized, res.Detail)
	require.NotContains(t, sent.Load().(string), "deepen",
		"no depth may be requested from a server that did not advertise shallow")
}

// Subscribe mode's entry point takes the same route.
func TestProbeInitializedOn_FullCloneWhenShallowNotAdvertised(t *testing.T) {
	srv, sent := nonShallowServed(t)
	requireNoShallowAdvertised(t, srv.URL)
	m := newTestManager(t)
	require.NoError(t, m.Start())

	res, err := m.ProbeInitializedOn(context.Background(), OriginSpec{URL: srv.URL}, "master")
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized, res.Detail)
	require.NotContains(t, sent.Load().(string), "deepen")
}

func TestProbeInitializedOn_UsesDepth1WhenShallowAdvertised(t *testing.T) {
	srv, sent := knomitServed(t)
	m := newTestManager(t)
	require.NoError(t, m.Start())

	res, err := m.ProbeInitializedOn(context.Background(), OriginSpec{URL: srv.URL}, "main")
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized, res.Detail)
	require.Contains(t, sent.Load().(string), "deepen 1")
}
