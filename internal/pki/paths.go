package pki

import "path/filepath"

// File names, instance side (inside [tls].dir, default <home>/pki). The
// instance KEY is not here: it stays at ~/.knomit/id_ed25519 (or
// [remote].ssh_key), the one copy.
const (
	InstanceCertFile = "instance.crt"
	RootCertFile     = "root.crt"
	CRLFile          = "crl.pem"
	// CRLNumberFile persists the highest CRL Number this instance has
	// accepted, so a restart cannot be fed an OLDER crl.pem and silently
	// un-revoke a serial.
	CRLNumberFile = "crl.number"
)

// File names, master side (the operator's --dir, never a fleet machine).
const (
	RootKeyFile    = "root.key"
	IssuedLogFile  = "issued.jsonl"
	RevokedLogFile = "revoked.jsonl"
)

// Dir is the default [tls].dir for a knomit home.
func Dir(home string) string { return filepath.Join(home, "pki") }
