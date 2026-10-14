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

// Revoked is one line of the fleet root's revocation log.
type Revoked struct {
	Serial *big.Int  `json:"serial"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// SignCRL signs a CRL listing revoked, with the given Number, ThisUpdate and
// NextUpdate, and returns its PEM. It writes nothing.
func SignCRL(root Root, revoked []Revoked, number *big.Int, thisUpdate, next time.Time) ([]byte, error) {
	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for _, r := range revoked {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: r.Serial, RevocationTime: r.At})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    number,
		ThisUpdate:                thisUpdate,
		NextUpdate:                next,
		RevokedCertificateEntries: entries,
	}, root.Cert, root.Signer)
	if err != nil {
		return nil, fmt.Errorf("pki: sign CRL: %w", err)
	}
	return crlPEM(der), nil
}

// IssueCRL is SignCRL as of time.Now() that also writes <dir>/crl.pem
// atomically. The PEM is returned too.
func IssueCRL(dir string, root Root, revoked []Revoked, number *big.Int, next time.Time) ([]byte, error) {
	out, err := SignCRL(root, revoked, number, time.Now(), next)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, CRLFile), out, 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

func crlPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// Revocation is one revocation as bytes. The caller places them: the
// statement and the CRL in the fleet repository, the line in the fleet root
// directory's local log.
type Revocation struct {
	// Statement is the root-signed revocation file (SignRevocation): the
	// fleet repository's per-operation record of this revocation.
	Statement []byte
	// CRLPEM is the CRL that replaces the current one: every serial the
	// current CRL lists, and r's.
	CRLPEM []byte
	Number *big.Int
	// Line is r as one line of the fleet root directory's LOCAL revocation
	// log (revoked.jsonl): JSON and a trailing newline. That log is the
	// master's own audit file; the fleet repository holds no ledger.
	Line []byte
}

// ErrAlreadyRevoked: the serial is already on the current CRL.
var ErrAlreadyRevoked = errors.New("pki: serial already revoked")

// RevokeSerial revokes r.Serial: it signs the revocation statement for it,
// and the CRL that lists every serial the CURRENT CRL lists (with their
// revocation times) AND r, numbered one above the current CRL's. current is
// the CRL now published, nil when there is none (the new CRL is then #1 and
// lists r alone); it must be signed by root. It writes nothing. The new CRL is
// built from the current CRL, never from a ledger, so it is a superset of the
// CRL it replaces: the fleet repository's crl-superset rule holds by
// construction. r.At is taken as UTC whole seconds (the statement's and the
// CRL's resolution); a serial already on the current CRL is ErrAlreadyRevoked.
func RevokeSerial(root Root, current *x509.RevocationList, r Revoked, thisUpdate, next time.Time) (Revocation, error) {
	if r.Serial == nil || r.Serial.Sign() <= 0 {
		return Revocation{}, errors.New("pki: revoke needs a positive serial")
	}
	if r.At.IsZero() {
		return Revocation{}, errors.New("pki: revoke needs a revocation time")
	}
	r.At = r.At.UTC().Truncate(time.Second)
	number := big.NewInt(1)
	var all []Revoked
	if current != nil {
		if _, err := CheckCRL(current, root.Cert, nil); err != nil {
			return Revocation{}, fmt.Errorf("pki: the current CRL: %w", err)
		}
		if IsRevoked(current, r.Serial) {
			return Revocation{}, fmt.Errorf("%w: %s", ErrAlreadyRevoked, r.Serial.Text(16))
		}
		number = new(big.Int).Add(current.Number, big.NewInt(1))
		for _, e := range current.RevokedCertificateEntries {
			all = append(all, Revoked{Serial: e.SerialNumber, At: e.RevocationTime})
		}
	}
	all = append(all, r)
	crl, err := SignCRL(root, all, number, thisUpdate, next)
	if err != nil {
		return Revocation{}, err
	}
	rootID, err := RootID(root.Cert)
	if err != nil {
		return Revocation{}, err
	}
	st, err := SignRevocation(root, RevocationStatement{Root: rootID, Serial: r.Serial, At: r.At, Reason: r.Reason})
	if err != nil {
		return Revocation{}, err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return Revocation{}, err
	}
	return Revocation{Statement: st, CRLPEM: crl, Number: number, Line: append(line, '\n')}, nil
}

// Revoke is RevokeSerial against the fleet root directory: it reissues
// <dir>/crl.pem from the CURRENT <dir>/crl.pem plus r, with a Number one
// higher (1 if there is none), and appends r to the local log
// <dir>/revoked.jsonl. The current crl.pem must list every serial the local
// log holds: a missing crl.pem while the log holds revocations, or a present
// one that lags it (an older CRL restored from a backup), is refused, as the
// CRL built from it would silently drop them. Nothing is written then.
func Revoke(dir string, root Root, r Revoked, next time.Time) (*big.Int, error) {
	prior, err := LoadRevoked(dir)
	if err != nil {
		return nil, err
	}
	var current *x509.RevocationList
	switch cur, err := LoadCRL(filepath.Join(dir, CRLFile)); {
	case err == nil:
		current = cur
		for _, p := range prior {
			if p.Serial == nil || !IsRevoked(current, p.Serial) {
				return nil, fmt.Errorf("pki: %s (Number %v) does not list serial %s, which %s records as revoked; refusing to sign a CRL that drops it (restore the newest CRL)", CRLFile, current.Number, p.Serial.Text(16), RevokedLogFile)
			}
		}
	case !errors.Is(err, ErrCRLMissing):
		return nil, fmt.Errorf("pki: current CRL unreadable, refusing to guess what it lists: %w", err)
	default:
		if len(prior) > 0 {
			return nil, fmt.Errorf("pki: %s is missing but %s lists %d revocations; refusing to sign a CRL that drops them", CRLFile, RevokedLogFile, len(prior))
		}
	}
	rv, err := RevokeSerial(root, current, r, time.Now(), next)
	if err != nil {
		return nil, err
	}
	if err := appendLine(filepath.Join(dir, RevokedLogFile), rv.Line); err != nil {
		return nil, fmt.Errorf("pki: revocation log: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, CRLFile), rv.CRLPEM, 0o644); err != nil {
		return nil, err
	}
	return rv.Number, nil
}

// LoadRevoked reads the fleet root's revocation log. An absent log is empty.
func LoadRevoked(dir string) ([]Revoked, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RevokedLogFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseRevokedLedger(raw)
}

// ParseRevokedLedger reads revocation ledger bytes (revoked.jsonl), from a
// file or from the fleet repo. Blank lines are skipped; any other line that
// is not a Revoked is an error.
func ParseRevokedLedger(raw []byte) ([]Revoked, error) {
	var out []Revoked
	sc := bufio.NewScanner(bytes.NewReader(raw))
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
