package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/pki"
)

// ensureKeyPair loads or generates an Ed25519 keypair at the given path.
// If the key does not exist, it is generated and the public key is logged to stderr.
// Returns the signer and the key's short fingerprint (first 8 hex chars of SHA256 of public key).
func ensureKeyPair(path string) (ssh.Signer, string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, "", fmt.Errorf("parse private key %s: %w", path, err)
		}
		fp := fingerprint(signer.PublicKey())
		return signer, fp, nil
	}

	if !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read key %s: %w", path, err)
	}

	// Generate new key
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, "", fmt.Errorf("mkdir for key: %w", err)
	}

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	pemBlock, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, "", fmt.Errorf("marshal private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	// Write private key atomically
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, pemBytes, 0600); err != nil {
		return nil, "", fmt.Errorf("write temp key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("rename key: %w", err)
	}

	// Write public key
	signer, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, "", fmt.Errorf("create signer: %w", err)
	}

	pubLine := pki.AuthorizedKeyLine(signer.PublicKey()) + "\n"

	pubPath := path + ".pub"
	if err := os.WriteFile(pubPath, []byte(pubLine), 0644); err != nil {
		return nil, "", fmt.Errorf("write public key: %w", err)
	}

	fp := fingerprint(signer.PublicKey())
	log.Info().Str("fingerprint", fp).Str("pub", pubPath).Msg("generated new SSH keypair")
	fmt.Fprintf(os.Stderr, "New knomit SSH public key: %s\n", strings.TrimSpace(pubLine))

	return signer, fp, nil
}

// fingerprint returns the first 8 hex chars of SHA256 of the public key.
func fingerprint(pub ssh.PublicKey) string {
	h := sha256.Sum256(pub.Marshal())
	return hex.EncodeToString(h[:])[:8]
}

// newIdentity BUILDS the identity a first boot persists, from this machine's
// hostname and the key's fingerprint8: host = the sanitized hostname,
// agent id = <host>-<fp8>, branch = agent/<agent id>. It is construction
// only. After the first boot the three are read back from control.db as
// stored (repos.ResolveIdentity), and nothing parses the host or the fp8 back
// out of the id or the branch (D18): they are separate stored values.
func newIdentity(fp string) repos.Identity {
	hostname, _ := os.Hostname()
	host := sanitizeHostname(hostname)
	id := host + "-" + fp
	return repos.Identity{AgentID: id, Branch: "agent/" + id, Host: host}
}

// sanitizeHostname lowercases the hostname — agent ids are lower case
// everywhere (pki.ValidAgentID), so the host part of a minted id is too, and
// the host label with it — and replaces chars invalid in git ref names with
// "-". Falls back to "local" if hostname is empty.
func sanitizeHostname(hostname string) string {
	if hostname == "" {
		return "local"
	}
	hostname = strings.ToLower(hostname)
	replacer := strings.NewReplacer(
		" ", "-",
		"~", "-",
		"^", "-",
		":", "-",
		"?", "-",
		"*", "-",
		"[", "-",
		"\\", "-",
		"+", "-", // an agent id may not hold + (it would read back as a shorter id)
		"@", "-",
	)
	return replacer.Replace(hostname)
}
