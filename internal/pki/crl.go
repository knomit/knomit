package pki

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrCRLMissing: there is no CRL to consult. Fail closed — an instance
	// that cannot tell whether a peer is revoked must not admit it.
	ErrCRLMissing = errors.New("pki: CRL missing")
	// ErrCRLInvalid: the CRL is malformed, not signed by the fleet root, or
	// older (lower Number) than one already accepted.
	ErrCRLInvalid = errors.New("pki: CRL invalid")
)

// Revoked is one line of the master's revocation log.
type Revoked struct {
	Serial *big.Int  `json:"serial"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// IssueCRL signs a CRL listing revoked, with the given Number and NextUpdate,
// and writes it to <dir>/crl.pem atomically. The PEM is returned too.
func IssueCRL(dir string, root Root, revoked []Revoked, number *big.Int, next time.Time) ([]byte, error) {
	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for _, r := range revoked {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: r.Serial, RevocationTime: r.At})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    number,
		ThisUpdate:                time.Now(),
		NextUpdate:                next,
		RevokedCertificateEntries: entries,
	}, root.Cert, root.Signer)
	if err != nil {
		return nil, fmt.Errorf("pki: sign CRL: %w", err)
	}
	out := crlPEM(der)
	if err := writeFileAtomic(filepath.Join(dir, CRLFile), out, 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

func crlPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// Revoke appends r to <dir>/revoked.jsonl and reissues <dir>/crl.pem from the
// WHOLE log with a Number one higher than the current crl.pem's (1 if none).
// The log is the master's revocation list; the CRL is a projection of it, so
// a reissue can never drop an earlier revocation.
func Revoke(dir string, root Root, r Revoked, next time.Time) (*big.Int, error) {
	if r.Serial == nil || r.Serial.Sign() <= 0 {
		return nil, errors.New("pki: revoke needs a positive serial")
	}
	number := big.NewInt(1)
	switch cur, err := LoadCRL(filepath.Join(dir, CRLFile)); {
	case err == nil:
		number = new(big.Int).Add(cur.Number, big.NewInt(1))
	case !errors.Is(err, ErrCRLMissing):
		return nil, fmt.Errorf("pki: current CRL unreadable, refusing to guess its Number: %w", err)
	}
	if err := appendJSONLine(filepath.Join(dir, RevokedLogFile), r); err != nil {
		return nil, fmt.Errorf("pki: revocation log: %w", err)
	}
	all, err := LoadRevoked(dir)
	if err != nil {
		return nil, err
	}
	if _, err := IssueCRL(dir, root, all, number, next); err != nil {
		return nil, err
	}
	return number, nil
}

// LoadRevoked reads the master's revocation log. An absent log is empty.
func LoadRevoked(dir string) ([]Revoked, error) {
	f, err := os.Open(filepath.Join(dir, RevokedLogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Revoked
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Revoked
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("pki: %s: %w", RevokedLogFile, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// LoadCRL reads a PEM CRL. Absent → ErrCRLMissing; unparseable → ErrCRLInvalid.
func LoadCRL(path string) (*x509.RevocationList, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrCRLMissing, path)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCRLInvalid, path, err)
	}
	return parseCRL(raw)
}

func parseCRL(raw []byte) (*x509.RevocationList, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "X509 CRL" {
		return nil, fmt.Errorf("%w: no X509 CRL PEM block", ErrCRLInvalid)
	}
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCRLInvalid, err)
	}
	return crl, nil
}

// CheckCRL verifies crl's signature against rootCert and that its Number is
// at least lastAccepted (nil accepts any). An expired NextUpdate is NOT an
// error: it returns stale=true, and the caller keeps enforcing the list and
// warns. Refusing a stale CRL would turn "the operator has not published in a
// while" into "every peer is refused", and accepting a stale one while
// IGNORING it would un-revoke everything; enforcing it is the only safe read.
func CheckCRL(crl *x509.RevocationList, rootCert *x509.Certificate, lastAccepted *big.Int) (stale bool, err error) {
	if crl == nil {
		return false, ErrCRLMissing
	}
	if err := crl.CheckSignatureFrom(rootCert); err != nil {
		return false, fmt.Errorf("%w: not signed by the fleet root: %v", ErrCRLInvalid, err)
	}
	if crl.Number == nil {
		return false, fmt.Errorf("%w: no CRL Number", ErrCRLInvalid)
	}
	if lastAccepted != nil && crl.Number.Cmp(lastAccepted) < 0 {
		return false, fmt.Errorf("%w: CRL Number %v is lower than the %v already accepted (rollback)", ErrCRLInvalid, crl.Number, lastAccepted)
	}
	return !crl.NextUpdate.IsZero() && time.Now().After(crl.NextUpdate), nil
}

// IsRevoked reports whether serial is listed in crl.
func IsRevoked(crl *x509.RevocationList, serial *big.Int) bool {
	for _, e := range crl.RevokedCertificateEntries {
		if e.SerialNumber != nil && e.SerialNumber.Cmp(serial) == 0 {
			return true
		}
	}
	return false
}

// RootID is the key the CRL watermark is filed under: pki.Fingerprint of the
// root's Ed25519 key — the one fingerprint definition, never the CommonName,
// which two unrelated roots can share. A non-Ed25519 root has no ID and is
// refused wherever a root is loaded.
func RootID(root *x509.Certificate) (string, error) {
	pub, ok := root.PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("pki: root %q key is %T, want ed25519", root.Subject.CommonName, root.PublicKey)
	}
	return Fingerprint(pub), nil
}

// watermarks is the on-disk form of crl.number: the highest CRL Number ever
// accepted, PER ROOT. A root's CRL numbering is its own; a different root
// starts over at 1. Keying by root means `install --replace-root` can adopt a
// new fleet's CRL #1 without forgetting the old fleet's watermark, so moving
// A -> B -> A with an OLD fleet-A bundle is still refused as a rollback.
type watermarks map[string]string // RootID -> decimal CRL Number

func readWatermarks(dir string) (watermarks, error) {
	raw, err := os.ReadFile(filepath.Join(dir, CRLNumberFile))
	if errors.Is(err, os.ErrNotExist) {
		return watermarks{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pki: read %s: %w", CRLNumberFile, err)
	}
	var w watermarks
	if err := json.Unmarshal(raw, &w); err != nil {
		// Garbled is an ERROR, not "none": resetting to accept-any is exactly
		// the rollback this file exists to stop.
		return nil, fmt.Errorf("pki: %s is not a CRL watermark record: %w", CRLNumberFile, err)
	}
	if w == nil {
		w = watermarks{}
	}
	for id, n := range w {
		if v, ok := new(big.Int).SetString(n, 10); !ok || v.Sign() < 0 {
			return nil, fmt.Errorf("pki: %s: bad CRL Number %q for root %s", CRLNumberFile, n, id)
		}
	}
	return w, nil
}

// AcceptedNumber returns the highest CRL Number this instance has accepted
// for root, or nil if it never accepted one from that root.
func AcceptedNumber(dir string, root *x509.Certificate) (*big.Int, error) {
	id, err := RootID(root)
	if err != nil {
		return nil, err
	}
	w, err := readWatermarks(dir)
	if err != nil {
		return nil, err
	}
	s, ok := w[id]
	if !ok {
		return nil, nil
	}
	n, _ := new(big.Int).SetString(s, 10)
	return n, nil
}

// RecordAcceptedNumber raises root's watermark to n (never lowers it) and
// keeps every other root's entry.
func RecordAcceptedNumber(dir string, root *x509.Certificate, n *big.Int) error {
	id, err := RootID(root)
	if err != nil {
		return err
	}
	w, err := readWatermarks(dir)
	if err != nil {
		return err
	}
	if cur, ok := w[id]; ok {
		if c, _ := new(big.Int).SetString(cur, 10); c.Cmp(n) >= 0 {
			return nil
		}
	}
	w[id] = n.String()
	raw, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, CRLNumberFile), append(raw, '\n'), 0o600)
}
