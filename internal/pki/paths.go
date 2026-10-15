package pki

// File names, instance side (inside [tls].dir; config.Load defaults it to
// <home>/pki, the one definition of that default). The
// instance KEY is not here: it stays at ~/.knomit/id_ed25519 (or
// [remote].ssh_key), the one copy.
const (
	InstanceCertFile = "instance.crt"
	RootCertFile     = "root.crt"
	CRLFile          = "crl.pem"
	// CRLNumberFile persists the highest CRL Number this instance has
	// accepted PER ROOT (a JSON object, RootID -> Number), so neither a
	// restart nor a root swap can be fed an OLDER crl.pem and silently
	// un-revoke a serial.
	CRLNumberFile = "crl.number"
)

// FleetRootDirName is the fleet root directory under the knomit home,
// <home>/fleet: the default --dir of `knomit identity`, and where knomit
// looks for the root key and certificate that make an instance its fleet's
// master.
const FleetRootDirName = "fleet"

// File names, fleet root side (the operator's --dir, never a fleet machine).
const (
	RootKeyFile    = "root.key"
	IssuedLogFile  = "issued.jsonl"
	RevokedLogFile = "revoked.jsonl"
)
