package pki

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// GitScheme is the go-git URL scheme for a git origin that is another
// enrolled knomit instance: knomit+https://<host>:<tls port>/git/<repo>.
// Forge clones keep using https://, whose go-git transport knomit never
// replaces. go-git parses the URL with url.Parse, which lowercases the
// scheme, so KNOMIT+HTTPS:// reaches this transport too; checks on the raw
// URL string elsewhere must be case-insensitive for the same reason.
const GitScheme = "knomit+https"

// ErrNotEnrolled: this instance has no fleet certificate, so it cannot fetch
// from a knomit+https origin.
var ErrNotEnrolled = errors.New("this instance has no fleet certificate; knomit+https origins need `knomit identity install`")

// fleetSource resolves the mTLS client LAZILY from the files under dir, so
// an instance enrolled after boot fetches on its next session without a
// restart, and an unenrolled one fails with ErrNotEnrolled before any
// connection is attempted.
type fleetSource struct {
	dir, keyPath string

	mu    sync.Mutex
	sum   [sha256.Size]byte
	hc    *http.Client
	inner transport.Transport // go-git's http transport over hc
}

// transport returns go-git's http transport over the fleet client, rebuilt
// when instance.crt, root.crt or crl.pem changed since the last session. A
// rebuild closes the previous client's idle connections, so a kept-alive
// connection to a peer that our new CRL revokes is not reused. It never
// returns a transport that connects without the fleet verifier: every
// failure is an error.
func (s *fleetSource) transport() (transport.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !HasInstanceCert(s.dir) {
		s.dropLocked()
		return nil, ErrNotEnrolled
	}
	h := sha256.New()
	for _, name := range []string{InstanceCertFile, RootCertFile, CRLFile} {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			s.dropLocked()
			return nil, fmt.Errorf("pki: %s transport unavailable: %w", GitScheme, err)
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	if s.inner != nil && bytes.Equal(sum[:], s.sum[:]) {
		return s.inner, nil
	}
	hc, err := HTTPClient(s.dir, s.keyPath)
	if err != nil {
		s.dropLocked()
		return nil, fmt.Errorf("pki: %s transport unavailable: %w", GitScheme, err)
	}
	s.dropLocked()
	s.sum, s.hc, s.inner = sum, hc, githttp.NewClient(hc)
	return s.inner, nil
}

func (s *fleetSource) dropLocked() {
	if s.hc != nil {
		s.hc.CloseIdleConnections()
	}
	s.hc, s.inner = nil, nil
}

// activeSource is what the registered transport resolves through. Tests in
// this package store their own; production stores it once, in
// InstallGitTransport.
var activeSource atomic.Pointer[fleetSource]

var install struct {
	mu           sync.Mutex
	installed    bool
	dir, keyPath string
}

// InstallGitTransport registers the fleet transport under GitScheme, once
// per process, resolving its client from dir and keyPath at each session.
// client.Protocols is an unsynchronised map: call from app.New before
// Manager.Start and before any goroutine that may clone or sync. Call it
// whether or not the instance is enrolled — the scheme must ALWAYS be
// recognised, so an unenrolled instance fails a knomit+https origin with
// ErrNotEnrolled instead of falling through to anything else.
//
// The returned error describes the state NOW, for the boot log: nil when the
// client builds, ErrNotEnrolled without a certificate, another error when the
// files are unusable. The registration stands in every case. A second call
// with the same files is a no-op; with other files it is an error and the
// first registration stands.
func InstallGitTransport(dir, keyPath string) error {
	install.mu.Lock()
	defer install.mu.Unlock()
	if install.installed {
		if install.dir != dir || install.keyPath != keyPath {
			return fmt.Errorf("pki: %s transport already installed for %q; not re-installing for %q", GitScheme, install.dir, dir)
		}
		return nil
	}
	src := &fleetSource{dir: dir, keyPath: keyPath}
	activeSource.Store(src)
	client.InstallProtocol(GitScheme, gitTransport{})
	install.installed, install.dir, install.keyPath = true, dir, keyPath
	_, err := src.transport()
	return err
}

// RewriteEndpoint copies ep with Protocol "https" so go-git's http transport
// builds a URL net/http accepts and its redirect guard compares https to
// https. The caller's endpoint is never mutated.
func RewriteEndpoint(ep *transport.Endpoint) *transport.Endpoint {
	c := *ep
	c.Protocol = "https"
	return &c
}

// gitTransport is what go-git finds under GitScheme. It holds no state: each
// session resolves the current fleet client through activeSource.
type gitTransport struct{}

func (gitTransport) inner(ep *transport.Endpoint, auth transport.AuthMethod) (transport.Transport, error) {
	if err := checkFleetEndpoint(ep, auth); err != nil {
		return nil, err
	}
	src := activeSource.Load()
	if src == nil {
		return nil, ErrNotEnrolled
	}
	return src.transport()
}

func (t gitTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	in, err := t.inner(ep, auth)
	if err != nil {
		return nil, err
	}
	s, err := in.NewUploadPackSession(RewriteEndpoint(ep), nil)
	if err != nil {
		return nil, ClassifyPeerError(err)
	}
	return upSession{s}, nil
}

func (t gitTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	in, err := t.inner(ep, auth)
	if err != nil {
		return nil, err
	}
	s, err := in.NewReceivePackSession(RewriteEndpoint(ep), nil)
	if err != nil {
		return nil, ClassifyPeerError(err)
	}
	return rpSession{s}, nil
}

// checkFleetEndpoint refuses what go-git would otherwise apply on top of the
// fleet client. ClientCert/ClientKey/CaBundle/InsecureSkipTLS make go-git
// clone the http.Transport and edit its tls.Config (common.go
// configureTransport), a proxy reroutes the connection, and a go-git
// AuthMethod or URL userinfo would send a credential — typically a forge
// token — to the peer. The credential on this scheme is the client
// certificate and nothing else.
func checkFleetEndpoint(ep *transport.Endpoint, auth transport.AuthMethod) error {
	switch {
	case ep.Protocol != GitScheme:
		return fmt.Errorf("pki: %s transport called for %q", GitScheme, ep.Protocol)
	case len(ep.ClientCert) > 0 || len(ep.ClientKey) > 0 || len(ep.CaBundle) > 0 || ep.InsecureSkipTLS:
		return fmt.Errorf("pki: %s origins verify with the fleet root and present the instance certificate; go-git TLS options are refused", GitScheme)
	case ep.Proxy.URL != "":
		return fmt.Errorf("pki: %s origins do not go through a proxy", GitScheme)
	case auth != nil:
		return fmt.Errorf("pki: %s origins authenticate with the instance certificate; a %s credential is refused", GitScheme, auth.Name())
	case ep.User != "" || ep.Password != "":
		return fmt.Errorf("pki: %s origins authenticate with the instance certificate; credentials in the URL are refused", GitScheme)
	}
	return nil
}

type upSession struct{ transport.UploadPackSession }

func (s upSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	ar, err := s.UploadPackSession.AdvertisedReferences()
	return ar, ClassifyPeerError(err)
}

func (s upSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	ar, err := s.UploadPackSession.AdvertisedReferencesContext(ctx)
	return ar, ClassifyPeerError(err)
}

func (s upSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	r, err := s.UploadPackSession.UploadPack(ctx, req)
	return r, ClassifyPeerError(err)
}

type rpSession struct{ transport.ReceivePackSession }

func (s rpSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	ar, err := s.ReceivePackSession.AdvertisedReferences()
	return ar, ClassifyPeerError(err)
}

func (s rpSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	ar, err := s.ReceivePackSession.AdvertisedReferencesContext(ctx)
	return ar, ClassifyPeerError(err)
}

func (s rpSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	r, err := s.ReceivePackSession.ReceivePack(ctx, req)
	return r, ClassifyPeerError(err)
}

// ClassifyPeerError keeps pki's named errors reachable by errors.Is through
// go-git and net/http.
//
// A REMOTE TLS alert — the peer refusing OUR certificate, which in TLS 1.3
// arrives only on the first read after the handshake — becomes
// ErrRefusedByPeer and nothing more specific: the alert does not say why
// (the reason is in the peer's log), so it is never mapped to ErrRevoked or
// ErrUntrustedRoot. Those two are OUR verifier's verdicts about the peer; they
// come out of VerifyConnection already in the chain and only need go-git's
// wrappers removed. go-git wraps request failures in plumbing.UnexpectedError
// and PermanentError, which have no Unwrap.
//
// A bare EOF is not mapped: nothing has shown it to be what a refused client
// sees against knomit's server, and naming it a refusal would blame the peer
// for any dropped connection. An error with no go-git wrapper and no alert is
// returned unchanged, so go-git's own sentinel comparisons
// (transport.ErrEmptyRemoteRepository and the like) still hold.
func ClassifyPeerError(err error) error {
	if err == nil {
		return nil
	}
	cause := err
	for {
		switch e := cause.(type) {
		case *plumbing.UnexpectedError:
			cause = e.Err
			continue
		case *plumbing.PermanentError:
			cause = e.Err
			continue
		}
		break
	}
	var op *net.OpError
	if errors.As(cause, &op) && op.Op == "remote error" {
		return fmt.Errorf("%w: %w", ErrRefusedByPeer, cause)
	}
	if cause != err {
		return shownAs{shown: err, cause: cause}
	}
	return err
}

// shownAs prints go-git's message and unwraps to the error it hid.
type shownAs struct{ shown, cause error }

func (e shownAs) Error() string { return e.shown.Error() }
func (e shownAs) Unwrap() error { return e.cause }
