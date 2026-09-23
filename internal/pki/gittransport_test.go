package pki

// The /git server here is a minimal upload-pack endpoint built from go-git's
// own plumbing/transport/server, NOT web.GitRemoteHandler: internal/web
// imports internal/pki (an import cycle), and internal/pki must stay pure Go
// with no cgo for tests.yml's no-model unit step. These tests prove the
// CLIENT transport; the real /git handler over knomit+https is proved in
// cmd/fleet_origin_test.go.
//
// SABOTAGE CHECKS (run against afbd3bab's code, before a comment-only amend;
// re-run by the reviewer at efc0318e; rerun after touching gittransport.go). Each turned exactly these red:
//   - ClassifyPeerError returning err unchanged: RevokedClient (a remote
//     alert is not ErrRefusedByPeer) and NamedErrorSurvivesGoGitsUnexpected-
//     Error. OtherRoot stays green: the advertisement request's error is a
//     *url.Error that unwraps, so no go-git wrapper hides ErrUntrustedRoot
//     there — the UploadPack path is the one that needs the classification.
//   - the upload-pack session not wrapped (constructor returns go-git's
//     session): the same two.
//   - RewriteEndpoint not rewriting Protocol: CloneAndFetch ("unsupported
//     protocol scheme") and RedirectStaysHTTPS.
//   - checkFleetEndpoint returning nil: TLSOptionsAndCredentialsAreRefused.
//   - (against f26b9208) a reinstall with other files returning early (no
//     swap): ReinstallWithOtherFilesSwapsTheIdentity (the server still sees
//     A) and cmd's ReinstallPresentsTheNewPrincipal; the map written on every
//     call: ReinstallWithOtherFilesSwapsTheIdentity (the sentinel entry is
//     replaced); the same-files early return removed:
//     ReinstallSameFilesIsANoOp (the source is replaced and a line logged).
//   - (against f9d7585c) the swap log emitted but the source NOT swapped:
//     ReinstallWithOtherFilesSwapsTheIdentity fails on the fingerprint line
//     (the server still sees A), before the log check is reached.
//   - the source hashing only instance.crt (no rebuild on a CRL change):
//     OurCRLChangeRebuildsTheClient (the kept-alive connection is reused).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// servedRepo is a repository on disk that a test server exposes at /git/kb.
type servedRepo struct {
	repo *git.Repository
	dir  string
}

func newServedRepo(t *testing.T) *servedRepo {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &servedRepo{repo: r, dir: dir}
	s.commit(t, "first")
	return s
}

func (s *servedRepo) commit(t *testing.T, msg string) plumbing.Hash {
	t.Helper()
	wt, err := s.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "t", Email: "t@example.invalid", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type oneRepo struct{ s storer.Storer }

func (l oneRepo) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

// gitHandler serves the two upload-pack endpoints of the smart HTTP protocol
// for one repository at /git/kb.
func gitHandler(s *servedRepo) http.Handler {
	srv := server.NewServer(oneRepo{s.repo.Storer})
	session := func(r *http.Request) (transport.UploadPackSession, error) {
		return srv.NewUploadPackSession(&transport.Endpoint{}, nil)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /git/kb/info/refs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != transport.UploadPackServiceName {
			http.Error(w, "upload-pack only", http.StatusForbidden)
			return
		}
		sess, err := session(r)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ar, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ar.Prefix = [][]byte{[]byte("# service=" + transport.UploadPackServiceName), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		ar.Encode(w)
	})
	mux.HandleFunc("POST /git/kb/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		req := packp.NewUploadPackRequest()
		if err := req.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		sess, err := session(r)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp, err := sess.UploadPack(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		resp.Encode(w)
	})
	return mux
}

// countingListener counts accepted TCP connections.
type countingListener struct {
	net.Listener
	n *atomic.Int32
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

var accepts = map[string]*atomic.Int32{}

// seenPeers[addr] is the fingerprint of the client certificate on the most
// recent request each test server answered.
var seenPeers = map[string]*atomic.Value{}

// startGitServer serves repo over a real TLS listener built from
// ServerConfig for the instance whose files are in dir. accepts[addr] counts
// its TCP connections.
func startGitServer(t *testing.T, dir, keyPath string, repo *servedRepo) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	cfg, err := ServerConfig(dir, keyPath, rec.logf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &atomic.Int32{}
	accepts[ln.Addr().String()] = n
	seen := &atomic.Value{}
	seenPeers[ln.Addr().String()] = seen
	inner := gitHandler(repo)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			seen.Store(Fingerprint(r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)))
		}
		inner.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler, ErrorLog: discardLogger()}
	go srv.Serve(tls.NewListener(countingListener{ln, n}, cfg))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), rec
}

// useSource makes the registered fleet transport resolve through a source
// for dir and keyPath for one test, and restores both afterwards. Tests in
// this file do not run in parallel: client.Protocols is a process-global,
// unsynchronised map.
func useSource(t *testing.T, dir, keyPath string) *fleetSource {
	t.Helper()
	prevEntry, had := client.Protocols[GitScheme]
	prevSrc := activeSource.Load()
	src := &fleetSource{dir: dir, keyPath: keyPath}
	activeSource.Store(src)
	client.InstallProtocol(GitScheme, gitTransport{})
	t.Cleanup(func() {
		activeSource.Store(prevSrc)
		if had {
			client.InstallProtocol(GitScheme, prevEntry)
		} else {
			client.InstallProtocol(GitScheme, nil)
		}
	})
	return src
}

// resetInstall clears the once-per-process install record for a test that
// exercises InstallGitTransport itself, and again afterwards.
func resetInstall(t *testing.T) {
	t.Helper()
	reset := func() {
		install.mu.Lock()
		defer install.mu.Unlock()
		install.installed, install.dir, install.keyPath = false, "", ""
		activeSource.Store(nil)
		client.InstallProtocol(GitScheme, nil)
	}
	reset()
	t.Cleanup(reset)
}

// closeIdle drops src's kept-alive connections so the next request
// handshakes, as they would be after the server's idle timeout.
func closeIdle(t *testing.T, src *fleetSource) {
	t.Helper()
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.hc == nil {
		t.Fatal("no client has been built yet")
	}
	src.hc.CloseIdleConnections()
}

func fleetURL(addr string) string { return GitScheme + "://" + addr + "/git/kb" }

// twoInstances is one fleet with a serving instance B and a fetching
// instance A, each with its own installed dir.
type twoInstances struct {
	f          fleet
	a, b       member
	aDir, bDir string
	repo       *servedRepo
	addr       string
	bLog       *recorder
}

func newTwoInstances(t *testing.T) *twoInstances {
	t.Helper()
	f := newFleet(t)
	a, aDir := f.clientSide(t, "alpha")
	b, bDir := f.clientSide(t, "bravo")
	repo := newServedRepo(t)
	addr, rec := startGitServer(t, bDir, b.keyPath, repo)
	return &twoInstances{f: f, a: a, b: b, aDir: aDir, bDir: bDir, repo: repo, addr: addr, bLog: rec}
}

// useA makes the fleet transport resolve through A's files.
func (x *twoInstances) useA(t *testing.T) *fleetSource {
	t.Helper()
	return useSource(t, x.aDir, x.a.keyPath)
}

func ctx10(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestGitTransport_CloneAndFetchOverKnomitHTTPS(t *testing.T) {
	x := newTwoInstances(t)
	x.useA(t)

	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)})
	if err != nil {
		t.Fatalf("clone: %v\nserver log:\n%s", err, x.bLog)
	}
	want, _ := x.repo.repo.Head()
	got, err := clone.Head()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != want.Hash() {
		t.Fatalf("cloned HEAD %s, server HEAD %s", got.Hash(), want.Hash())
	}

	second := x.repo.commit(t, "second")
	if err := clone.FetchContext(ctx10(t), &git.FetchOptions{}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ref, err := clone.Reference(plumbing.NewRemoteReferenceName("origin", want.Name().Short()), true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != second {
		t.Fatalf("fetched %s, want the server's new commit %s", ref.Hash(), second)
	}
}

// The server (B) revokes A; A's CRL does not list B, so only B's refusal
// can fail the fetch. In TLS 1.3 that refusal is an alert on A's first read.
// The SAME source, as the sync loop holds it: revocation is checked at the
// handshake, so the kept-alive connection from before it is dropped first,
// as the server's idle timeout would.
func TestGitTransport_RevokedClientIsRefusedWithNamedError(t *testing.T) {
	x := newTwoInstances(t)
	src := x.useA(t)
	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)})
	if err != nil { // positive control: A was accepted before the revocation
		t.Fatalf("clone: %v\nserver log:\n%s", err, x.bLog)
	}
	x.f.Revoke(t, x.a, x.bDir)
	closeIdle(t, src)
	x.repo.commit(t, "after revocation")
	err = clone.FetchContext(ctx10(t), &git.FetchOptions{})
	if !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("err=%v, want ErrRefusedByPeer\nserver log:\n%s", err, x.bLog)
	}
	if errors.Is(err, ErrRevoked) {
		t.Fatalf("a remote alert was named ErrRevoked; only our own verifier can say that: %v", err)
	}
	if !x.bLog.has(ErrRevoked.Error()) {
		t.Fatalf("server did not log why:\n%s", x.bLog)
	}
}

// OUR CRL changing (A revokes B) rebuilds the client, so the connection kept
// alive from before is not reused and the next fetch handshakes into A's
// verifier. No closeIdle here: the rebuild is what forces the handshake.
func TestGitTransport_OurCRLChangeRebuildsTheClient(t *testing.T) {
	x := newTwoInstances(t)
	x.useA(t)
	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	x.f.Revoke(t, x.b, x.aDir)
	x.repo.commit(t, "after revocation")
	err = clone.FetchContext(ctx10(t), &git.FetchOptions{})
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("err=%v, want ErrRevoked from our own verifier", err)
	}
}

func TestGitTransport_OtherRootServerIsRefused(t *testing.T) {
	f := newFleet(t)
	a, aDir := f.clientSide(t, "alpha")
	other := newFleet(t)
	b, bDir := other.clientSide(t, "bravo")
	addr, _ := startGitServer(t, bDir, b.keyPath, newServedRepo(t))
	useSource(t, aDir, a.keyPath)

	_, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)})
	if !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

// Also through UploadPack, where go-git wraps the failure in
// plumbing.UnexpectedError (no Unwrap): the advertisement succeeds, then A
// revokes B and the pack request handshakes again.
func TestGitTransport_NamedErrorSurvivesGoGitsUnexpectedError(t *testing.T) {
	x := newTwoInstances(t)
	src := x.useA(t)
	ep, err := transport.NewEndpoint(fleetURL(x.addr))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := gitTransport{}.NewUploadPackSession(ep, nil)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := sess.AdvertisedReferencesContext(ctx10(t))
	if err != nil {
		t.Fatalf("advertise: %v", err)
	}
	x.f.Revoke(t, x.b, x.aDir) // A now refuses B
	closeIdle(t, src)
	req := packp.NewUploadPackRequestFromCapabilities(ar.Capabilities)
	head, _ := x.repo.repo.Head()
	req.Wants = append(req.Wants, head.Hash())
	_, err = sess.UploadPack(ctx10(t), req)
	var ue *plumbing.UnexpectedError
	if errors.As(err, &ue) {
		t.Fatalf("go-git's UnexpectedError reached the caller unclassified: %v", err)
	}
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("err=%v, want ErrRevoked", err)
	}
}

func TestGitTransport_HTTPSProtocolEntryUntouched(t *testing.T) {
	resetInstall(t)
	x := newTwoInstances(t)
	before := client.Protocols["https"]
	beforeHTTP := client.Protocols["http"]
	if err := InstallGitTransport(x.aDir, x.a.keyPath); err != nil {
		t.Fatal(err)
	}
	if client.Protocols["https"] != before || client.Protocols["http"] != beforeHTTP {
		t.Fatal("installing the fleet transport replaced go-git's http(s) transport, which carries forge traffic")
	}
	if _, ok := client.Protocols[GitScheme].(gitTransport); !ok {
		t.Fatalf("%s is %T, want the fleet transport", GitScheme, client.Protocols[GitScheme])
	}
}

// go-git lowercases the scheme, so an upper-case URL reaches the fleet
// transport and is served over mTLS, not refused as an unknown scheme.
func TestGitTransport_SchemeIsCaseInsensitive(t *testing.T) {
	x := newTwoInstances(t)
	x.useA(t)
	if _, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: "KNOMIT+HTTPS://" + x.addr + "/git/kb"}); err != nil {
		t.Fatalf("clone: %v", err)
	}
}

// notListening is an address whose listener fails the test if anything
// connects.
func notListening(t *testing.T) (string, func() bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Store(true)
			c.Close()
		}
	}()
	return ln.Addr().String(), func() bool { ln.Close(); <-done; return accepted.Load() }
}

func TestGitTransport_NotEnrolledFailsNamedWithoutConnecting(t *testing.T) {
	resetInstall(t)
	key, _ := writeOpenSSHKey(t)
	if err := InstallGitTransport(filepath.Join(t.TempDir(), "pki"), key); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("install reported %v, want ErrNotEnrolled", err)
	}
	addr, accepted := notListening(t)
	_, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)})
	if accepted() {
		t.Fatal("an unenrolled instance opened a connection to a knomit+https origin")
	}
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err=%v, want ErrNotEnrolled", err)
	}
}

// Installed at boot unenrolled; `knomit identity install` later; the next
// clone works with no second install and no restart.
func TestGitTransport_EnrolledAfterBootWorksWithoutRestart(t *testing.T) {
	resetInstall(t)
	f := newFleet(t)
	b, bDir := f.clientSide(t, "bravo")
	repo := newServedRepo(t)
	addr, _ := startGitServer(t, bDir, b.keyPath, repo)

	a := f.enroll(t, "alpha")
	aDir := filepath.Join(t.TempDir(), "pki")
	if err := InstallGitTransport(aDir, a.keyPath); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("install reported %v, want ErrNotEnrolled", err)
	}
	if _, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("before enrolment: err=%v, want ErrNotEnrolled", err)
	}
	copyDir(t, f.install(t, a), aDir) // what `knomit identity install` leaves in aDir
	if _, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)}); err != nil {
		t.Fatalf("after enrolment: %v", err)
	}
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{InstanceCertFile, RootCertFile, CRLFile} {
		b, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(filepath.Join(to, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A certificate whose CRL is missing cannot build a verifier; the scheme is
// still registered and fails with the reason, without connecting.
func TestGitTransport_UnbuildableClientFailsWithoutConnecting(t *testing.T) {
	resetInstall(t)
	f := newFleet(t)
	a, aDir := f.clientSide(t, "alpha")
	if err := os.Remove(filepath.Join(aDir, CRLFile)); err != nil {
		t.Fatal(err)
	}
	if err := InstallGitTransport(aDir, a.keyPath); err == nil || errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("install reported %v, want the missing-CRL error", err)
	}
	addr, accepted := notListening(t)
	_, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)})
	if accepted() {
		t.Fatal("connected without a verifier")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err=%v, want the missing crl.pem", err)
	}
}

func TestGitTransport_RedirectStaysHTTPS(t *testing.T) {
	ep, err := transport.NewEndpoint(fleetURL("h.example:8443"))
	if err != nil {
		t.Fatal(err)
	}
	c := RewriteEndpoint(ep)
	if c.Protocol != "https" || c.String() != "https://h.example:8443/git/kb" {
		t.Fatalf("copy is %q (%s), want https://h.example:8443/git/kb", c.String(), c.Protocol)
	}
	if ep.Protocol != GitScheme {
		t.Fatalf("the caller's endpoint was mutated to %q", ep.Protocol)
	}
}

func TestGitTransport_InstallIsIdempotentAndRaceFree(t *testing.T) {
	resetInstall(t)
	x := newTwoInstances(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = InstallGitTransport(x.aDir, x.a.keyPath)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	src := activeSource.Load()
	if _, ok := client.Protocols[GitScheme].(gitTransport); !ok || src == nil || src.dir != x.aDir {
		t.Fatalf("%s is %T with source %+v", GitScheme, client.Protocols[GitScheme], src)
	}
}

// The same dir and keyPath again is a no-op: same source, no map write, no log.
func TestGitTransport_ReinstallSameFilesIsANoOp(t *testing.T) {
	resetInstall(t)
	x := newTwoInstances(t)
	if err := InstallGitTransport(x.aDir, x.a.keyPath); err != nil {
		t.Fatal(err)
	}
	src := activeSource.Load()
	sentinel := &gitTransportSentinel{}
	client.Protocols[GitScheme] = sentinel // any write by the next call would replace it
	logs := captureZerolog(t)
	if err := InstallGitTransport(x.aDir, x.a.keyPath); err != nil {
		t.Fatalf("same-files reinstall: %v", err)
	}
	if client.Protocols[GitScheme] != sentinel {
		t.Fatal("a same-files reinstall wrote go-git's protocol map")
	}
	if activeSource.Load() != src {
		t.Fatal("a same-files reinstall replaced the source")
	}
	if logs.Len() != 0 {
		t.Fatalf("a same-files reinstall logged: %s", logs)
	}
}

// Another dir and keyPath SWAPS the client source without writing go-git's
// map again, logs one line naming both dirs, and the next fetch presents the
// NEW identity to the server.
func TestGitTransport_ReinstallWithOtherFilesSwapsTheIdentity(t *testing.T) {
	resetInstall(t)
	x := newTwoInstances(t)
	c, cDir := x.f.clientSide(t, "charlie")

	if err := InstallGitTransport(x.aDir, x.a.keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)}); err != nil {
		t.Fatalf("clone as A: %v", err)
	}
	if got := seenPeers[x.addr].Load(); got != Fingerprint(x.a.pub) {
		t.Fatalf("server saw %v, want A %s", got, Fingerprint(x.a.pub))
	}

	registered := client.Protocols[GitScheme]
	sentinel := &gitTransportSentinel{}
	client.Protocols[GitScheme] = sentinel
	logs := captureZerolog(t)
	if err := InstallGitTransport(cDir, c.keyPath); err != nil {
		t.Fatalf("install as C: %v", err)
	}
	if client.Protocols[GitScheme] != sentinel {
		t.Fatal("the swap wrote go-git's protocol map a second time")
	}
	client.Protocols[GitScheme] = registered
	if _, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)}); err != nil {
		t.Fatalf("clone as C: %v", err)
	}
	if got := seenPeers[x.addr].Load(); got != Fingerprint(c.pub) {
		t.Fatalf("after the swap the server saw %v, want C %s (A is %s)", got, Fingerprint(c.pub), Fingerprint(x.a.pub))
	}

	// Only after the identity is proven at the server: the one log line.
	// Decode rather than search the bytes: zerolog writes JSON, which escapes
	// a Windows path's backslashes, so the raw path never appears verbatim.
	var line struct {
		OldDir string `json:"old_dir"`
		NewDir string `json:"new_dir"`
	}
	if n := bytes.Count(logs.Bytes(), []byte("\n")); n != 1 {
		t.Fatalf("want one log line, got %d:\n%s", n, logs)
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.OldDir != x.aDir || line.NewDir != cDir {
		t.Fatalf("log line %s: old_dir=%q new_dir=%q (err %v), want %q and %q", logs, line.OldDir, line.NewDir, err, x.aDir, cDir)
	}
}

// gitTransportSentinel stands in go-git's map to detect a write.
type gitTransportSentinel struct{ failingSessions }

type failingSessions struct{}

func (failingSessions) NewUploadPackSession(*transport.Endpoint, transport.AuthMethod) (transport.UploadPackSession, error) {
	return nil, errors.New("sentinel")
}

func (failingSessions) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, errors.New("sentinel")
}

func captureZerolog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// go-git options that would edit the fleet tls.Config, or send a credential
// to the peer, are refused before any connection.
func TestGitTransport_TLSOptionsAndCredentialsAreRefused(t *testing.T) {
	x := newTwoInstances(t)
	x.useA(t)
	for name, opts := range map[string]*git.CloneOptions{
		"InsecureSkipTLS": {URL: fleetURL(x.addr), InsecureSkipTLS: true},
		"CABundle":        {URL: fleetURL(x.addr), CABundle: []byte("x")},
		"basic auth":      {URL: fleetURL(x.addr), Auth: &githttp.BasicAuth{Username: "x-token", Password: "ghp_secret"}},
		"URL userinfo":    {URL: GitScheme + "://x-token:ghp_secret@" + x.addr + "/git/kb"},
	} {
		_, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, opts)
		if err == nil {
			t.Errorf("%s: clone succeeded", name)
		}
	}
	if n := accepts[x.addr].Load(); n != 0 {
		t.Fatalf("the server accepted %d connections; refusals must come before any dial", n)
	}
}

func TestIsFleetURL(t *testing.T) {
	for u, want := range map[string]bool{
		"knomit+https://h/git/kb": true,
		"KNOMIT+HTTPS://h/git/kb": true,
		"Knomit+Https://h/git/kb": true,
		"https://h/git/kb":        false,
		"knomit+http://h/git/kb":  false,
		"knomit+https:/h":         false,
		"":                        false,
		"xknomit+https://h":       false,
	} {
		if got := IsFleetURL(u); got != want {
			t.Errorf("IsFleetURL(%q) = %v, want %v", u, got, want)
		}
	}
}
