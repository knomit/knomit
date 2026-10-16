package pki

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClientConfig is the tls.Config for connecting to another enrolled instance:
// it presents this instance's certificate and verifies the peer's with
// VerifyInstanceChain (UsageServer) against the fleet root and CRL in dir.
//
// It FAILS CLOSED at construction: no instance certificate, no root, a
// missing or malformed CRL, or a certificate for another key is an error, so
// a config whose verifier has nothing to verify against cannot be built. The
// three files are re-read on every handshake (the reloader the server uses),
// so a long-lived client picks up a newly published CRL without a rebuild.
func ClientConfig(dir, keyPath string) (*tls.Config, error) {
	r, _, err := newReloader(dir, keyPath, nil)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(r.cur.root)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// InsecureSkipVerify turns off crypto/tls's OWN verification, which
		// would demand a DNS or IP SAN matching the dialed host — knomit
		// certificates carry only a knomit:// URI SAN — and it is safe ONLY
		// because VerifyConnection below is the complete verifier: chain to
		// the fleet root alone, ServerAuth EKU, CRL, SAN shape, fingerprint,
		// role. Every path through it that cannot verify returns an error.
		InsecureSkipVerify: true,
		// RootCAs is INERT here: with InsecureSkipVerify set, crypto/tls
		// never consults it. It is set to the fleet root for the reader; the
		// fleet-root-only guarantee lives in the pool VerifyInstanceChain
		// builds (the root and nothing else, never SystemCertPool).
		RootCAs: pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c := r.refresh().cert
			return &c, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			return r.verify(r.refresh(), cs, UsageServer)
		},
	}, nil
}

// HTTPClient is an http.Client that speaks mTLS to enrolled instances.
func HTTPClient(dir, keyPath string) (*http.Client, error) {
	cfg, err := ClientConfig(dir, keyPath)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:     cfg,
		TLSHandshakeTimeout: 10 * time.Second, // defensive bound on a dead peer, not a measured value
	}}, nil
}

// Dial connects to addr with mTLS and returns the peer's VERIFIED Identity.
// It is what F10 (peers) and F13 (host probe) will call; in phase 2 only
// tests call it. The connection is closed before returning.
func Dial(ctx context.Context, addr, dir, keyPath string) (Identity, error) {
	cfg, err := ClientConfig(dir, keyPath)
	if err != nil {
		return Identity{}, err
	}
	d := tls.Dialer{Config: cfg}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Identity{}, err
	}
	defer c.Close()
	tc, ok := c.(*tls.Conn)
	if !ok {
		return Identity{}, errors.New("pki: dial did not return a TLS connection")
	}
	cs := tc.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return Identity{}, fmt.Errorf("%w: peer presented no certificate", ErrUntrustedRoot)
	}
	if err := peerAccepted(ctx, tc, addr); err != nil {
		return Identity{}, err
	}
	// VerifyConnection accepted the chain during the handshake; this only
	// reads what it established.
	return IdentityOf(cs.PeerCertificates[0])
}

// peerAccepted proves the PEER accepted our certificate, which a finished
// handshake does not: in TLS 1.3 the client completes its side before the
// server has judged the client certificate, and a refusal arrives only as an
// alert on the first read. So Dial completes one minimal HTTP/1.1 exchange —
// any status line means the peer let us in; a remote TLS alert means it
// refused us (the reason is in ITS log, not ours).
func peerAccepted(ctx context.Context, tc *tls.Conn, addr string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second) // defensive bound on a peer that never answers
	}
	if err := tc.SetDeadline(deadline); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if _, err := fmt.Fprintf(tc, "HEAD / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		return peerError(err)
	}
	line, err := bufio.NewReader(tc).ReadString('\n')
	if err != nil {
		return peerError(err)
	}
	if !strings.HasPrefix(line, "HTTP/") {
		return fmt.Errorf("pki: %s answered the handshake but is not an HTTP server: %q", addr, strings.TrimSpace(line))
	}
	return nil
}

// peerError maps a remote TLS alert — crypto/tls reports one as a
// *net.OpError whose Op is "remote error" — to ErrRefusedByPeer.
func peerError(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return fmt.Errorf("%w: %v", ErrRefusedByPeer, op.Err)
	}
	return fmt.Errorf("pki: peer did not answer after the handshake: %w", err)
}

// ErrOriginUnreachable: ClassifyOrigin could not connect to the origin's
// host, so it could not decide whether the origin is an instance of this
// fleet. The caller refuses the origin rather than guessing.
var ErrOriginUnreachable = errors.New("pki: can't reach the origin")

// ErrOriginCertRefused: ClassifyOrigin reached the origin, and our verifier
// refused the certificate it presents as one of this fleet's (revoked,
// expired, a CRL or key failure). It always wraps the verdict. With
// ErrRefusedByPeer (the origin refusing OUR certificate) it is the only
// classification error that means the origin answered with a refusal: every
// other one (no TCP connection, a handshake or an answer that failed for a
// reason that is no verdict, this instance's own trust files) is a failure
// to reach a verdict, and a caller tells the operator so.
var ErrOriginCertRefused = errors.New("pki: the origin presents a certificate of this fleet that is refused")

// ClassifyOrigin decides whether rawURL is an instance of THIS fleet: a
// server whose certificate VerifyInstanceChain accepts against the root
// installed in dir, and which accepts this instance's certificate. It is
// asked ONCE, when an origin is set; the answer is stored as the origin's
// auth method (cert), and nothing re-decides it.
//
// Every answer fails closed:
//   - not an https:// URL (http, ssh, git@, a path): not a peer, no dial.
//   - no root.crt in dir (this instance holds no fleet trust): not a peer,
//     no dial.
//   - unusable trust files (ClientConfig fails): the error.
//   - no TCP connection (refused, DNS, timeout): ErrOriginUnreachable.
//   - our verifier said ErrUntrustedRoot (a forge, another fleet): not a
//     peer. Stock https then verifies against the system pool, which never
//     holds a fleet root, so a misjudged peer still fails at its handshake,
//     before any request or credential is sent.
//   - any other verdict of our verifier (ErrRevoked, ErrExpired, a CRL or
//     key error): ErrOriginCertRefused wrapping it; the peer refusing our
//     certificate: ErrRefusedByPeer. Never "not a peer", so a revoked peer
//     can never be re-classified onto stock https.
//   - a handshake that failed before our verifier ran (no TLS 1.3, not TLS,
//     no certificate): not a peer.
//
// It uses ClientConfig, the one config the no-InsecureSkipVerify invariant
// allows; against a forge, verification fails before our client certificate
// is sent, and no HTTP request is ever made.
func ClassifyOrigin(ctx context.Context, dir, keyPath, rawURL string) (bool, error) {
	if hasScheme(rawURL, gitScheme) {
		return false, fmt.Errorf("%w (%s)", ErrUnsupportedOriginScheme, rawURL)
	}
	if !hasScheme(rawURL, "https") {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dir, RootCertFile)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("pki: read the installed fleet root: %w", err)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false, fmt.Errorf("pki: %q is not a URL with a host", rawURL)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}
	cfg, err := ClientConfig(dir, keyPath)
	if err != nil {
		return false, fmt.Errorf("pki: this instance's fleet trust files are unusable, so the origin %s cannot be judged: %w", addr, err)
	}
	if net.ParseIP(u.Hostname()) == nil {
		cfg.ServerName = u.Hostname()
	}
	verify := cfg.VerifyConnection
	var verified bool
	var verdict error
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		verified = true
		verdict = verify(cs)
		return verdict
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second) // defensive bound on a host that never answers
		defer cancel()
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, fmt.Errorf("%w: can't reach %s: %v", ErrOriginUnreachable, addr, err)
	}
	defer raw.Close()
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		switch {
		case verified && errors.Is(verdict, ErrUntrustedRoot):
			return false, nil
		case verified && verdict != nil:
			return false, fmt.Errorf("%w (%s): %w", ErrOriginCertRefused, addr, verdict)
		case verified:
			return false, fmt.Errorf("pki: the origin %s presents a certificate of this fleet, but the handshake failed: %w", addr, err)
		case ctx.Err() != nil:
			return false, fmt.Errorf("%w: can't reach %s: %v", ErrOriginUnreachable, addr, err)
		}
		return false, nil
	}
	if err := peerAccepted(ctx, tc, addr); err != nil {
		return false, fmt.Errorf("pki: the origin %s is an instance of this fleet, but: %w", addr, err)
	}
	return true, nil
}
