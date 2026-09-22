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
