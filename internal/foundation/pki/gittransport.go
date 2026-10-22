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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/rs/zerolog/log"
)

// gitScheme is the go-git URL scheme of the fleet transport. It is an
// INTERNAL key: go-git picks a transport by URL scheme alone, so an origin
// stored with auth method cert (an https URL that classified as a fleet peer,
// ClassifyOrigin) is handed to go-git as gitScheme://… by GitURL, and only
// there and in the derived .git/config. Nothing a user types, no API value and
// no control.db row carries it. go-git parses the URL with url.Parse, which
// lowercases the scheme.
const gitScheme = "knomit+https"

// ErrUnsupportedOriginScheme: an origin URL written with the fleet
// transport's internal scheme, as input or found stored. A fleet peer is
// named by its plain https URL.
var ErrUnsupportedOriginScheme = errors.New("not a supported origin URL: use https://")

// hasScheme reports whether u starts with scheme:// (case-insensitive, as
// go-git's url.Parse reads it).
func hasScheme(u, scheme string) bool {
	prefix := scheme + "://"
	return len(u) >= len(prefix) && strings.EqualFold(u[:len(prefix)], prefix)
}

// GitURL is the URL go-git is handed for an origin: for a fleet peer (an
// origin stored with auth method cert) the same https URL under the internal
// scheme, so go-git dispatches it to the fleet transport; for any other
// origin the URL unchanged. It is the ONE place that reads or writes the
// internal scheme on a URL: a URL that already carries it is refused
// (ErrUnsupportedOriginScheme), which closes the scheme to input and to
// anything stored, and a peer URL that is not https:// is refused.
func GitURL(u string, fleetPeer bool) (string, error) {
	if hasScheme(u, gitScheme) {
		return "", fmt.Errorf("%w (%s)", ErrUnsupportedOriginScheme, u)
	}
	if !fleetPeer {
		return u, nil
	}
	if !hasScheme(u, "https") {
		return "", fmt.Errorf("pki: an origin served by the fleet transport must be an https:// URL, not %q", u)
	}
	return gitScheme + u[len("https"):], nil
}

// ErrNotEnrolled: this instance holds no fleet certificate, so it cannot
// fetch from an origin that is a fleet instance.
var ErrNotEnrolled = errors.New("this instance holds no fleet certificate, so fleet origins cannot be fetched (join a fleet: Fleet → Create or join a fleet)")

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
			return nil, fmt.Errorf("pki: fleet transport unavailable: %w", err)
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
		return nil, fmt.Errorf("pki: fleet transport unavailable: %w", err)
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

// InstallGitTransport registers the fleet transport under gitScheme with
// go-git EXACTLY ONCE per process, and points it at a client source for dir
// and keyPath that is resolved at each session. client.Protocols is an
// unsynchronised map: the first call must come from app.New before
// Manager.Start and before any goroutine that may clone or sync. Call it
// whether or not the instance is enrolled — the scheme must ALWAYS be
// recognised, so an unenrolled instance fails a fleet origin with
// ErrNotEnrolled instead of falling through to anything else.
//
// A later call with the SAME dir and keyPath is a no-op. A later call with a
// DIFFERENT dir or keyPath swaps the client source (an atomic pointer; the
// go-git registration is not touched again) and closes the old source's
// idle connections, so the next session presents the new identity. It never
// silently keeps the old one.
//
// The returned error describes the state NOW, for the boot log: nil when the
// client builds, ErrNotEnrolled without a certificate, another error when the
// files are unusable. The registration stands in every case.
func InstallGitTransport(dir, keyPath string) error {
	install.mu.Lock()
	defer install.mu.Unlock()
	if install.installed && install.dir == dir && install.keyPath == keyPath {
		return nil
	}
	src := &fleetSource{dir: dir, keyPath: keyPath}
	old := activeSource.Swap(src)
	if old != nil {
		old.mu.Lock()
		old.dropLocked()
		old.mu.Unlock()
		log.Info().Str("old_dir", old.dir).Str("new_dir", dir).
			Msg("fleet transport: client identity swapped to other pki files")
	}
	if !install.installed {
		client.InstallProtocol(gitScheme, gitTransport{})
	}
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

// gitTransport is what go-git finds under gitScheme. It holds no state: each
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
	case ep.Protocol != gitScheme:
		return fmt.Errorf("pki: the fleet transport called for %q", ep.Protocol)
	case len(ep.ClientCert) > 0 || len(ep.ClientKey) > 0 || len(ep.CaBundle) > 0 || ep.InsecureSkipTLS:
		return errors.New("pki: fleet origins verify with the fleet root and present the instance certificate; go-git TLS options are refused")
	case ep.Proxy.URL != "":
		return errors.New("pki: fleet origins do not go through a proxy")
	case auth != nil:
		return fmt.Errorf("pki: fleet origins authenticate with the instance certificate; a %s credential is refused", auth.Name())
	case ep.User != "" || ep.Password != "":
		return errors.New("pki: fleet origins authenticate with the instance certificate; credentials in the URL are refused")
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
